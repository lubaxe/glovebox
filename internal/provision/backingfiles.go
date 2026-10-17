package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// BackingfilesLabel is the XFS label of the partition holding the cam disk
// and snapshots.
const BackingfilesLabel = "backingfiles"

// Backingfiles makes sure an XFS filesystem with reflink support is mounted
// at mnt. On a freshly flashed card it creates a partition for it in the
// free space after the last partition of the disk holding the root
// filesystem; the image leaves that space unpartitioned on purpose.
type Backingfiles struct {
	Mountpoint string
	Fstab      string // usually /etc/fstab
	// Disk overrides which disk to partition (tests); empty means the disk
	// holding /.
	Disk    string
	MinSize int64 // smallest free space worth using
	// Label is the filesystem label; empty means BackingfilesLabel.
	Label  string
	Runner system.Runner
}

func (b *Backingfiles) label() string {
	if b.Label != "" {
		return b.Label
	}
	return BackingfilesLabel
}

// Ensure mounts the backingfiles filesystem, creating it if needed.
func (b *Backingfiles) Ensure(ctx context.Context) (created bool, err error) {
	if _, err := b.Runner.Run(ctx, "mountpoint", "-q", b.Mountpoint); err == nil {
		return false, nil // already mounted
	}
	dev := ""
	if out, err := b.Runner.Run(ctx, "blkid", "-L", b.label()); err == nil {
		dev = strings.TrimSpace(string(out))
	}
	if dev == "" {
		if dev, err = b.create(ctx); err != nil {
			return false, err
		}
		created = true
	}
	if err := b.addFstab(); err != nil {
		return created, err
	}
	if err := os.MkdirAll(b.Mountpoint, 0o755); err != nil {
		return created, err
	}
	if _, err := b.Runner.Run(ctx, "mount", "-t", "xfs", "-o", "noatime", dev, b.Mountpoint); err != nil {
		return created, fmt.Errorf("mounting %s: %w", dev, err)
	}
	return created, nil
}

// sfdisk --json output, the parts we use.
type partitionTable struct {
	PartitionTable struct {
		Label      string `json:"label"`
		SectorSize int64  `json:"sectorsize"`
		Partitions []struct {
			Node  string `json:"node"`
			Start int64  `json:"start"`
			Size  int64  `json:"size"`
			Type  string `json:"type"`
		} `json:"partitions"`
	} `json:"partitiontable"`
}

// create adds a partition in the free space at the end of the disk and
// formats it. It refuses anything unexpected rather than guess.
func (b *Backingfiles) create(ctx context.Context) (string, error) {
	disk := b.Disk
	if disk == "" {
		var err error
		if disk, err = b.rootDisk(ctx); err != nil {
			return "", err
		}
	}
	out, err := b.Runner.Run(ctx, "sfdisk", "--json", disk)
	if err != nil {
		return "", fmt.Errorf("reading partition table of %s: %w", disk, err)
	}
	var pt partitionTable
	if err := json.Unmarshal(out, &pt); err != nil {
		return "", fmt.Errorf("parsing partition table of %s: %w", disk, err)
	}
	t := pt.PartitionTable
	if t.Label != "dos" {
		return "", fmt.Errorf("%s has a %q partition table; expected the image's MBR (dos)", disk, t.Label)
	}
	sector := t.SectorSize
	if sector == 0 {
		sector = 512
	}
	// A third partition with no filesystem is ours from a start that was
	// cut short before mkfs finished (power loss, service restart).
	if n := len(t.Partitions); n == 3 {
		last := t.Partitions[n-1]
		part := partitionNode(disk, n)
		if last.Type == "83" && last.Size*sector >= b.MinSize && !b.hasFilesystem(ctx, part) {
			return part, b.format(ctx, part)
		}
	}
	if len(t.Partitions) >= 3 {
		return "", fmt.Errorf("%s already has %d partitions and none is labelled %s", disk, len(t.Partitions), b.label())
	}
	var end int64 // first sector after the last partition
	for _, p := range t.Partitions {
		end = max(end, p.Start+p.Size)
	}
	start := (end + 2047) / 2048 * 2048 // 1 MiB aligned

	size, err := b.diskSectors(ctx, disk, sector)
	if err != nil {
		return "", err
	}
	if free := (size - start) * sector; free < b.MinSize {
		return "", fmt.Errorf("only %d MiB free after the last partition of %s; need at least %d MiB",
			max(free, 0)>>20, disk, b.MinSize>>20)
	}

	ir, ok := b.Runner.(system.InputRunner)
	if !ok {
		return "", errors.New("runner can't feed sfdisk its script")
	}
	// start given, no size: use the rest of the disk; type 83 = Linux
	script := fmt.Sprintf("start=%d, type=83\n", start)
	if _, err := ir.RunInput(ctx, script, "sfdisk", "--append", "--no-reread", "--no-tell-kernel", disk); err != nil {
		return "", fmt.Errorf("adding partition to %s: %w", disk, err)
	}
	nr := len(t.Partitions) + 1
	part := partitionNode(disk, nr)
	// The disk is in use, so tell the kernel about just the new partition.
	// If the disk isn't busy, udev may already have re-read the table, and
	// partx then fails because the partition exists; that's fine.
	if _, err := b.Runner.Run(ctx, "partx", "--add", "--nr", strconv.Itoa(nr), disk); err != nil {
		if werr := waitFor(ctx, part, 3*time.Second); werr != nil {
			return "", fmt.Errorf("registering the new partition: %w", err)
		}
	}
	if err := waitFor(ctx, part, 10*time.Second); err != nil {
		return "", err
	}
	return part, b.format(ctx, part)
}

// format makes the XFS filesystem. -K skips discarding every block first,
// which takes minutes on a large card and gains nothing on a new partition.
func (b *Backingfiles) format(ctx context.Context, part string) error {
	if _, err := b.Runner.Run(ctx, "mkfs.xfs", "-f", "-K", "-m", "reflink=1", "-L", b.label(), part); err != nil {
		return fmt.Errorf("formatting %s: %w", part, err)
	}
	return nil
}

// hasFilesystem reports whether blkid finds any signature on part. When in
// doubt it says yes, so nothing gets formatted by mistake.
func (b *Backingfiles) hasFilesystem(ctx context.Context, part string) bool {
	out, err := b.Runner.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", part)
	if system.ExitCode(err) == 2 {
		return false // nothing found
	}
	return err != nil || strings.TrimSpace(string(out)) != ""
}

// rootDisk returns the disk holding the root filesystem, e.g. /dev/mmcblk0.
func (b *Backingfiles) rootDisk(ctx context.Context) (string, error) {
	out, err := b.Runner.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "/")
	if err != nil {
		return "", err
	}
	src := strings.TrimSpace(string(out))
	out, err = b.Runner.Run(ctx, "lsblk", "-n", "-o", "PKNAME", src)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("can't tell which disk %s is on", src)
	}
	return "/dev/" + name, nil
}

func (b *Backingfiles) diskSectors(ctx context.Context, disk string, sector int64) (int64, error) {
	out, err := b.Runner.Run(ctx, "blockdev", "--getsize64", disk)
	if err != nil {
		return 0, err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", disk, err)
	}
	return bytes / sector, nil
}

func (b *Backingfiles) addFstab() error {
	data, err := os.ReadFile(b.Fstab)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == b.Mountpoint {
			return nil
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(fmt.Sprintf("LABEL=%s %s xfs noatime,nofail 0 2\n", b.label(), b.Mountpoint))...)
	return os.WriteFile(b.Fstab, data, 0o644)
}

// partitionNode names partition nr of disk: /dev/sda -> /dev/sda3,
// /dev/mmcblk0 or /dev/loop4 -> /dev/mmcblk0p3, /dev/loop4p3.
func partitionNode(disk string, nr int) string {
	if c := disk[len(disk)-1]; c >= '0' && c <= '9' {
		return fmt.Sprintf("%sp%d", disk, nr)
	}
	return fmt.Sprintf("%s%d", disk, nr)
}

func waitFor(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
