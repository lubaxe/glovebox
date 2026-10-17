// Package storage creates and checks the disk images presented to the car,
// and the XFS filesystem that holds them.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lubaxe/glovebox/internal/loopdev"
	"github.com/lubaxe/glovebox/internal/system"
)

// CreateDiskImage creates a sparse disk image of size bytes at path with an
// MBR partition table and one exFAT partition labelled label, containing an
// empty folder (e.g. TeslaCam; "" for none). It refuses to overwrite an
// existing file.
func CreateDiskImage(ctx context.Context, r system.Runner, path string, size int64, label, folder string) (err error) {
	size -= size % (1 << 20) // whole MiB
	table, err := mbr(size, partTypeExFAT)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating disk image: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	if err := f.Truncate(size); err != nil {
		f.Close()
		return fmt.Errorf("sizing %s: %w", path, err)
	}
	if _, err := f.WriteAt(table, 0); err != nil {
		f.Close()
		return fmt.Errorf("writing partition table: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}

	dev, err := loopdev.Attach(ctx, r, path, false)
	if err != nil {
		return err
	}
	defer func() {
		if derr := dev.Detach(context.WithoutCancel(ctx)); err == nil {
			err = derr
		}
	}()
	part, err := dev.WaitPartition(ctx, 1)
	if err != nil {
		return err
	}
	if _, err := r.Run(ctx, "mkfs.exfat", "-L", label, part); err != nil {
		return fmt.Errorf("formatting %s: %w", path, err)
	}

	if folder == "" {
		return nil
	}
	// e.g. the car needs a TeslaCam folder to record into
	mnt, err := os.MkdirTemp("", "glovebox-newdisk-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if _, err := r.Run(ctx, "mount", "-t", "exfat", part, mnt); err != nil {
		return fmt.Errorf("mounting new disk image: %w", err)
	}
	mkErr := os.Mkdir(filepath.Join(mnt, folder), 0o755)
	if _, err := r.Run(ctx, "umount", mnt); err != nil {
		return fmt.Errorf("unmounting new disk image: %w", err)
	}
	if mkErr != nil {
		return fmt.Errorf("creating %s folder: %w", folder, mkErr)
	}
	return nil
}

// Maintenance is what MaintainDiskImage did.
type Maintenance struct {
	Repaired bool // fsck fixed errors
	Trimmed  bool // fstrim released free space back to the backing filesystem
}

// MaintainDiskImage repairs the image at path with fsck, mounts it
// read-write, runs work on the mounted filesystem, and finally trims it so
// space freed on the drive is freed in the backing file too. If fsck can't
// repair the filesystem, work isn't run. Only call it while the image isn't
// presented to the car.
func MaintainDiskImage(ctx context.Context, r system.Runner, path string, work func(mnt string) error) (m Maintenance, err error) {
	bg := context.WithoutCancel(ctx) // cleanup must finish even if ctx ends
	dev, err := loopdev.Attach(ctx, r, path, false)
	if err != nil {
		return m, err
	}
	defer func() {
		if derr := dev.Detach(bg); err == nil {
			err = derr
		}
	}()
	part, err := dev.WaitPartition(ctx, 1)
	if err != nil {
		return m, err
	}
	if m.Repaired, err = Fsck(ctx, r, part); err != nil {
		return m, err
	}
	if work == nil {
		return m, nil
	}

	mnt, err := os.MkdirTemp("", "glovebox-cam-")
	if err != nil {
		return m, err
	}
	defer os.Remove(mnt)
	if _, err := r.Run(ctx, "mount", part, mnt); err != nil {
		return m, fmt.Errorf("mounting %s: %w", path, err)
	}
	defer func() {
		if _, uerr := r.Run(bg, "umount", mnt); uerr != nil && err == nil {
			err = fmt.Errorf("unmounting %s: %w", path, uerr)
		}
	}()
	if err := work(mnt); err != nil {
		return m, err
	}
	// best effort: older kernels can't trim exFAT
	_, terr := r.Run(ctx, "fstrim", mnt)
	m.Trimmed = terr == nil
	return m, nil
}

// Fsck repairs the filesystem on part automatically (fsck -p) and reports
// whether anything was fixed.
func Fsck(ctx context.Context, r system.Runner, part string) (repaired bool, err error) {
	_, err = r.Run(ctx, "fsck", part, "--", "-p")
	switch system.ExitCode(err) {
	case -1:
		if err != nil {
			return false, err
		}
		return false, nil
	case 0:
		return false, nil
	case 1, 2: // errors corrected (2: "reboot", which only matters for /)
		return true, nil
	default:
		return false, fmt.Errorf("fsck %s: %w", part, err)
	}
}

// ErrNoReflink means the filesystem can't make copy-on-write clones.
var ErrNoReflink = errors.New("filesystem doesn't support reflink copies")

// CheckReflink verifies that dir is on a filesystem that supports
// copy-on-write clones, which snapshots depend on.
func CheckReflink(dir string) error {
	src, err := os.CreateTemp(dir, ".reflink-check-")
	if err != nil {
		return err
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.WriteString("glovebox"); err != nil {
		return err
	}
	dst := src.Name() + ".clone"
	defer os.Remove(dst)
	if err := Clone(src.Name(), dst); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	return nil
}
