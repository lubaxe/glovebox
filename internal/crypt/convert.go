package crypt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/lubaxe/glovebox/internal/bootfs"
	"github.com/lubaxe/glovebox/internal/system"
)

// Conversion turns the plain backingfiles partition into an encrypted one.
// XFS can't shrink, so it can't be encrypted in place: everything on it is
// lost, the car's drive included. It runs at startup, before anything
// uses backingfiles, when Service.TakeRequest says it was asked for.
type Conversion struct {
	// Mountpoint is where backingfiles is mounted, e.g. /backingfiles.
	Mountpoint string
	// Fstab loses its line for Mountpoint: Glovebox mounts the volume
	// itself once it's open.
	Fstab string
	// SnapshotRoot is where snapshots are mounted, e.g.
	// /run/glovebox/snapshots; they're unmounted first.
	SnapshotRoot string
	Volume       *Volume
	Recovery     Recovery
	Runner       system.Runner
	// Release lets go of the filesystem first, e.g. disconnects the car's
	// drive.
	Release          func(ctx context.Context) error
	writeReservation func(context.Context, io.Writer, int64) error
}

// Run encrypts the partition and leaves the new volume open and mounted at
// Mountpoint, with a fresh XFS filesystem labelled "backingfiles".
func (c *Conversion) Run(ctx context.Context) error {
	return bootfs.WithWrite(ctx, c.Recovery.BootWriter, func() error {
		return c.run(ctx)
	})
}

func (c *Conversion) run(ctx context.Context) (err error) {
	recipient, err := c.Recovery.Recipient()
	if err != nil {
		return err
	}
	if recipient == "" {
		return errors.New("no recovery key is set")
	}
	dev := c.Volume.Device
	if dev == "" {
		if dev, err = c.partition(ctx); err != nil {
			return err
		}
	}
	if err := c.Recovery.preflight(); err != nil {
		return fmt.Errorf("checking recovery output access: %w", err)
	}
	if err := EnsureKey(c.Volume.KeyFile); err != nil {
		return fmt.Errorf("preparing the device key: %w", err)
	}
	reservation, err := reserveRecoverySpace(ctx, c.Recovery.Dir, c.writeReservation)
	if err != nil {
		return fmt.Errorf("reserving recovery output space: %w", err)
	}
	defer func() { err = errors.Join(err, reservation.release()) }()
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.Release != nil {
		if err := c.Release(ctx); err != nil {
			return fmt.Errorf("disconnecting the drive: %w", err)
		}
	}
	if err := c.unmountSnapshots(ctx); err != nil {
		return err
	}
	if _, err := c.Runner.Run(ctx, "mountpoint", "-q", c.Mountpoint); err == nil {
		if _, err := c.Runner.Run(ctx, "umount", c.Mountpoint); err != nil {
			return fmt.Errorf("unmounting %s: %w", c.Mountpoint, err)
		}
	}
	// From here on the old filesystem is gone. Should anything fail, the
	// next start finds a partition without a filesystem and makes a new
	// plain one (see provision.Backingfiles), so the car gets a drive.
	if _, err := c.Runner.Run(ctx, "wipefs", "-a", dev); err != nil {
		return fmt.Errorf("clearing %s: %w", dev, err)
	}
	if err := c.Volume.Format(ctx, dev); err != nil {
		return err
	}
	c.Volume.Device = dev
	if err := c.Volume.Open(ctx); err != nil {
		return fmt.Errorf("opening the new volume: %w", err)
	}
	key, err := c.Volume.VolumeKey(ctx)
	if err != nil {
		return err
	}
	defer clear(key)
	if err := reservation.release(); err != nil {
		return fmt.Errorf("releasing recovery output space: %w", err)
	}
	if err := c.Recovery.saveVolumeKey(recipient, key); err != nil {
		return fmt.Errorf("saving the sealed volume key: %w", err)
	}
	if err := c.Volume.BackupHeader(ctx, c.Recovery.HeaderPath()); err != nil {
		return fmt.Errorf("backing up the header: %w", err)
	}
	if _, err := c.Runner.Run(ctx, "mkfs.xfs", "-f", "-K", "-m", "reflink=1", "-L", "backingfiles", c.Volume.Mapper()); err != nil {
		return fmt.Errorf("formatting the new volume: %w", err)
	}
	if err := c.removeFstab(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.Mountpoint, 0o755); err != nil {
		return err
	}
	if _, err := c.Runner.Run(ctx, "mount", "-t", "xfs", "-o", "noatime", c.Volume.Mapper(), c.Mountpoint); err != nil {
		return fmt.Errorf("mounting the new volume: %w", err)
	}
	return nil
}

// partition returns the partition mounted at Mountpoint, refusing anything
// that isn't a whole partition of a disk (a loop file, say).
func (c *Conversion) partition(ctx context.Context) (string, error) {
	out, err := c.Runner.Run(ctx, "findmnt", "-n", "-o", "SOURCE", c.Mountpoint)
	if err != nil {
		return "", fmt.Errorf("%s isn't mounted: %w", c.Mountpoint, err)
	}
	dev := strings.TrimSpace(string(out))
	out, err = c.Runner.Run(ctx, "lsblk", "-n", "-d", "-o", "TYPE", dev)
	if err != nil {
		return "", err
	}
	if t := strings.TrimSpace(string(out)); t != "part" {
		return "", fmt.Errorf("%s is on %s, a %q device; only a partition of the card can be encrypted", c.Mountpoint, dev, t)
	}
	return dev, nil
}

// unmountSnapshots unmounts everything under SnapshotRoot and detaches the
// loop devices of files on backingfiles.
func (c *Conversion) unmountSnapshots(ctx context.Context) error {
	if c.SnapshotRoot != "" {
		out, err := c.Runner.Run(ctx, "findmnt", "-rn", "-o", "TARGET")
		if err != nil {
			return err
		}
		root := path.Clean(c.SnapshotRoot) + "/"
		for _, t := range strings.Split(string(out), "\n") {
			if t = strings.TrimSpace(t); strings.HasPrefix(t, root) {
				if _, err := c.Runner.Run(ctx, "umount", t); err != nil {
					return fmt.Errorf("unmounting snapshot %s: %w", t, err)
				}
			}
		}
	}
	out, err := c.Runner.Run(ctx, "losetup", "-n", "-l", "-O", "NAME,BACK-FILE")
	if err != nil {
		return err
	}
	prefix := path.Clean(c.Mountpoint) + "/"
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[1], prefix) {
			if _, err := c.Runner.Run(ctx, "losetup", "-d", f[0]); err != nil {
				return fmt.Errorf("detaching %s: %w", f[0], err)
			}
		}
	}
	return nil
}

func (c *Conversion) removeFstab() error {
	data, err := os.ReadFile(c.Fstab)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == c.Mountpoint {
			continue
		}
		keep = append(keep, line)
	}
	return writeAtomic(c.Fstab, []byte(strings.Join(keep, "\n")), 0o644)
}
