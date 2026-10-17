// Package loopdev attaches disk image files as loop devices.
package loopdev

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// Device is an attached loop device whose partitions have been scanned.
type Device struct {
	Path string // e.g. /dev/loop3
	r    system.Runner
}

// Attach attaches file to a free loop device and scans its partition table,
// so the first partition shows up as Partition(1).
func Attach(ctx context.Context, r system.Runner, file string, readOnly bool) (*Device, error) {
	args := []string{"--find", "--show", "--partscan"}
	if readOnly {
		args = append(args, "--read-only")
	}
	out, err := r.Run(ctx, "losetup", append(args, file)...)
	if err != nil {
		return nil, fmt.Errorf("attaching %s: %w", file, err)
	}
	dev := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dev) {
		return nil, fmt.Errorf("attaching %s: unexpected losetup output %q", file, dev)
	}
	return &Device{Path: dev, r: r}, nil
}

// Partition returns the device node of partition n, e.g. /dev/loop3p1.
func (d *Device) Partition(n int) string {
	return fmt.Sprintf("%sp%d", d.Path, n)
}

// WaitPartition waits for partition n's device node to appear, which udev
// does shortly after Attach.
func (d *Device) WaitPartition(ctx context.Context, n int) (string, error) {
	part := d.Partition(n)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(part); err == nil {
			return part, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not appear", part)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Detach releases the loop device.
func (d *Device) Detach(ctx context.Context) error {
	if _, err := d.r.Run(ctx, "losetup", "--detach", d.Path); err != nil {
		return fmt.Errorf("detaching %s: %w", d.Path, err)
	}
	return nil
}

// AttachedTo returns the loop devices that file is currently attached to.
func AttachedTo(ctx context.Context, r system.Runner, file string) ([]*Device, error) {
	out, err := r.Run(ctx, "losetup", "--noheadings", "--output", "NAME", "--associated", file)
	if err != nil {
		return nil, fmt.Errorf("finding loop devices for %s: %w", file, err)
	}
	var devs []*Device
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			devs = append(devs, &Device{Path: name, r: r})
		}
	}
	return devs, nil
}
