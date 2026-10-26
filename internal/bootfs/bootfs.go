// Package bootfs coordinates temporary writes to an exact vfat boot mount.
// It does not protect other filesystems or guarantee whole-card power-loss safety.
package bootfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

const cleanupTimeout = 10 * time.Second

// Writer provides a serialized writable scope. Callbacks must finish before
// returning and must not recursively acquire the same writer.
type Writer interface {
	WithWrite(context.Context, func() error) error
}

// WithWrite delegates to writer, or calls fn directly when writer is nil.
// An already-canceled context prevents either action.
func WithWrite(ctx context.Context, writer Writer, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer == nil {
		return fn()
	}
	return writer.WithWrite(ctx, fn)
}

// Manager serializes WithWrite and Protect operations within this instance.
// All boot writers must share one Manager; external processes and other Manager
// instances are not coordinated. Construct it with New and do not copy it.
type Manager struct {
	mountpoint string
	runner     system.Runner
	gate       chan struct{}
}

// New constructs a Manager for an absolute, clean, non-root Linux mountpoint.
// Invalid paths are rejected by operations, without running commands. A successful
// findmnt JSON listing is filtered by exact target, never by a containing filesystem.
// Missing mounts and exact non-vfat mounts are unmanaged: WithWrite calls fn
// without remounting or flushing, and Protect does nothing. Query failures,
// malformed listings, ambiguous mounts, and noncanonical matching targets fail closed.
func New(mountpoint string, runner system.Runner) *Manager {
	return &Manager{mountpoint: mountpoint, runner: runner, gate: make(chan struct{}, 1)}
}

// WithWrite acquires a cancelable scope and detects the original mount state.
// For vfat, it remounts ro to rw, runs fn, flushes with sync -f, and restores
// original ro even after callback/flush errors, cancellation, or a callback panic.
// An originally rw mount remains rw. Cleanup commands each use a separate
// ten-second context detached from caller cancellation; errors are joined.
// A failed writable remount prevents fn from running but still triggers cleanup,
// since the command may have changed the mount before reporting failure.
// Callbacks must observe
// cancellation themselves; this method cannot interrupt fn.
func (m *Manager) WithWrite(ctx context.Context, fn func() error) (err error) {
	if err = m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	managed, readOnly, err := m.state(ctx)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !managed {
		return fn()
	}
	defer func() {
		cleanupErr := m.cleanup(ctx, readOnly)
		err = errors.Join(err, ctx.Err(), cleanupErr)
	}()
	if readOnly {
		if err = m.remount(ctx, "rw"); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Protect serializes with writable scopes and makes an exact rw vfat mount ro,
// flushing first. Already-ro and unmanaged mounts are left alone. Once a rw
// mount is detected, both flush and remount are attempted despite cancellation
// or flush failure, using the same bounded cleanup policy as WithWrite.
func (m *Manager) Protect(ctx context.Context) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	managed, readOnly, err := m.state(ctx)
	if err != nil {
		return err
	}
	if !managed || readOnly {
		return ctx.Err()
	}
	err = m.cleanup(ctx, true)
	return errors.Join(err, ctx.Err())
}

func (m *Manager) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() { <-m.gate }

func (m *Manager) state(ctx context.Context) (bool, bool, error) {
	if !path.IsAbs(m.mountpoint) || m.mountpoint == "/" || path.Clean(m.mountpoint) != m.mountpoint || strings.ContainsAny(m.mountpoint, "\\\x00") {
		return false, false, fmt.Errorf("bootfs: unsafe mountpoint %q", m.mountpoint)
	}
	if m.runner == nil {
		return false, false, errors.New("bootfs: nil runner")
	}
	output, err := m.runner.Run(ctx, "findmnt", "--json", "--list", "--output", "TARGET,FSTYPE,OPTIONS")
	if err != nil {
		return false, false, errors.Join(fmt.Errorf("bootfs: findmnt: %w", err), ctx.Err())
	}
	var listing struct {
		Filesystems *[]struct {
			Target  string `json:"target"`
			FSType  string `json:"fstype"`
			Options string `json:"options"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal(output, &listing); err != nil {
		return false, false, fmt.Errorf("bootfs: decode findmnt: %w", err)
	}
	if listing.Filesystems == nil {
		return false, false, errors.New("bootfs: findmnt missing filesystems array")
	}
	found, managed, readOnly := false, false, false
	for _, mount := range *listing.Filesystems {
		if !path.IsAbs(mount.Target) || mount.FSType == "" || mount.Options == "" {
			return false, false, errors.New("bootfs: malformed findmnt mount entry")
		}
		if path.Clean(mount.Target) == m.mountpoint && mount.Target != m.mountpoint {
			return false, false, fmt.Errorf("bootfs: unexpected mount target %q", mount.Target)
		}
		if mount.Target != m.mountpoint {
			continue
		}
		if found {
			return false, false, fmt.Errorf("bootfs: ambiguous mount target %q", m.mountpoint)
		}
		found = true
		if mount.FSType != "vfat" {
			continue
		}
		managed = true
		modes := 0
		for _, option := range strings.Split(mount.Options, ",") {
			if option == "ro" || option == "rw" {
				modes++
				readOnly = option == "ro"
			}
		}
		if modes != 1 {
			return false, false, fmt.Errorf("bootfs: ambiguous mount options %q", mount.Options)
		}
	}
	return managed, readOnly, nil
}

func (m *Manager) remount(ctx context.Context, mode string) error {
	if _, err := m.runner.Run(ctx, "mount", "-o", "remount,"+mode, m.mountpoint); err != nil {
		return fmt.Errorf("bootfs: remount %s: %w", mode, err)
	}
	return nil
}

func (m *Manager) cleanup(ctx context.Context, restore bool) error {
	flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	_, flushErr := m.runner.Run(flushCtx, "sync", "-f", m.mountpoint)
	cancelFlush()
	if flushErr != nil {
		flushErr = fmt.Errorf("bootfs: sync: %w", flushErr)
	}
	var restoreErr error
	if restore {
		restoreCtx, cancelRestore := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		restoreErr = m.remount(restoreCtx, "ro")
		cancelRestore()
	}
	return errors.Join(flushErr, restoreErr)
}
