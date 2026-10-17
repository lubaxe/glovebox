// Package drive runs the car's drive: it prepares the cam disk image,
// presents it over USB, and snapshots it on a schedule.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/storage"
	"github.com/lubaxe/glovebox/internal/system"
)

// Gadget is the part of gadget.Gadget the drive uses.
type Gadget interface {
	Enable(ctx context.Context, luns []gadget.LUN) error
	Disable(ctx context.Context) error
	Eject(lun int) error
	Load(lun int, file string) error
	State() (gadget.State, error)
}

// Snapshots is the part of snapshot.Manager the drive uses.
type Snapshots interface {
	Take(ctx context.Context) (snapshot.Snapshot, error)
	List() ([]snapshot.Snapshot, error)
	Release(ctx context.Context, id string) error
	Restore(ctx context.Context) error
	Compact(ctx context.Context) (int, error)
}

// Drive coordinates the cam disk, the USB gadget and snapshots.
type Drive struct {
	Backingfiles string
	CamSize      int64
	Interval     time.Duration
	MinFree      int64
	// SpaceCheck is how often free space is checked between snapshots.
	SpaceCheck time.Duration
	// EjectPause is how long an extra drive stays ejected while its new
	// version is swapped in.
	EjectPause time.Duration
	// KeepFreePercent is how much of the cam drive the boot-time cleanup
	// tries to keep free by deleting archived events.
	KeepFreePercent int
	// Archived reports whether a cam drive file is safely archived; nil
	// means nothing is archived, so no events are ever deleted.
	Archived func(rel string, size int64) bool
	// Extras are presented after the cam drive, in this order.
	Extras []Extra

	Gadget    Gadget
	Snapshots Snapshots
	Runner    system.Runner
	Log       *slog.Logger

	// Hooks for tests.
	checkReflink   func(dir string) error
	createImage    func(ctx context.Context, r system.Runner, path string, size int64, label, folder string) error
	clone          func(src, dst string) error
	allocated      func(path string) (int64, error)
	maintainImage  func(ctx context.Context, r system.Runner, path string, work func(mnt string) error) (storage.Maintenance, error)
	diskSpace      func(path string) (total, free int64, err error)
	freeBytes      func(path string) (int64, error)
	snapshotSignal chan chan error // nil: no one waits for the result

	mu          sync.Mutex
	lastErr     string
	lastCleanup *Cleanup
	extraStatus map[string]*ExtraStatus
	syncMu      sync.Mutex // one extra drive sync at a time
}

// Status is what the API reports about the drive.
type Status struct {
	CamDisk   string       `json:"cam_disk"`
	CamSize   int64        `json:"cam_size_bytes"`
	Gadget    gadget.State `json:"gadget"`
	Snapshots int          `json:"snapshots"`
	FreeBytes int64        `json:"backingfiles_free_bytes"`
	LastError string       `json:"last_error,omitempty"`
	// Cleanup is what the cleanup at startup did, if it ran.
	Cleanup *Cleanup `json:"cleanup,omitempty"`
	// Extras are the music, light show and Boombox drives that are on.
	Extras []ExtraStatus `json:"extras"`
}

// New returns a Drive using the real system.
func New(backingfiles string, camSize int64, interval time.Duration, minFree int64,
	g Gadget, snaps Snapshots, r system.Runner, log *slog.Logger) *Drive {
	return &Drive{
		Backingfiles: backingfiles, CamSize: camSize, Interval: interval, MinFree: minFree,
		KeepFreePercent: 25, SpaceCheck: 30 * time.Second, EjectPause: 5 * time.Second,
		Gadget: g, Snapshots: snaps, Runner: r, Log: log,
		checkReflink:   storage.CheckReflink,
		createImage:    storage.CreateDiskImage,
		clone:          storage.Clone,
		allocated:      storage.Allocated,
		maintainImage:  storage.MaintainDiskImage,
		diskSpace:      storage.DiskSpace,
		freeBytes:      storage.FreeBytes,
		snapshotSignal: make(chan chan error, 1),
	}
}

// CamDisk is the path of the image presented to the car.
func (d *Drive) CamDisk() string { return filepath.Join(d.Backingfiles, "cam_disk.bin") }

// Start prepares the cam disk and presents it to the car. It returns once
// the drive is available; snapshots keep running until ctx is done.
func (d *Drive) Start(ctx context.Context) error {
	if err := d.checkReflink(d.Backingfiles); err != nil {
		return fmt.Errorf("backingfiles at %s can't be used for snapshots: %w", d.Backingfiles, err)
	}
	st, err := d.Gadget.State()
	if err != nil {
		return err
	}
	if st.Enabled && (!slices.Equal(st.Files, d.lunFiles()) || !d.extrasReady()) {
		// The drives were changed in the config. Only reconnecting adds or
		// removes one, which the car takes like the Pi restarting.
		d.Log.Info("the set of drives changed; reconnecting them", "was", st.Files, "now", d.lunFiles())
		if err := d.Gadget.Disable(ctx); err != nil {
			return err
		}
		st.Enabled, st.Files = false, nil
	}
	d.removeUnusedExtras(st.Files)
	if st.Enabled {
		// The service restarted while the drive stayed connected; the car may
		// be writing to the image, so leave it alone.
		d.Log.Info("drive already presented to the car, not checking the cam disk", "host_state", st.HostState)
	} else {
		if err := d.prepareCamDisk(ctx); err != nil {
			return err
		}
		if err := d.prepareExtras(ctx); err != nil {
			// the dashcam matters more than music
			d.Log.Error("extra drives not available", "err", err)
			d.setError(err)
			d.Extras = nil
		}
		// Present the drives before anything else: until then the car can't
		// record.
		luns := []gadget.LUN{{File: d.CamDisk(), Product: "CAM"}}
		for _, x := range d.Extras {
			luns = append(luns, gadget.LUN{File: d.ExtraDisk(x.Name), Product: x.Label})
		}
		if err := d.Gadget.Enable(ctx, luns); err != nil {
			return err
		}
		d.Log.Info("presenting drives over USB", "files", d.lunFiles())
	}

	// Remounting snapshots doesn't touch the live image, so it can wait
	// until the car has its drive.
	if err := d.Snapshots.Restore(ctx); err != nil {
		return fmt.Errorf("restoring snapshots: %w", err)
	}

	go func() {
		// remove duplicate snapshots left from before Take skipped them
		if n, err := d.Snapshots.Compact(ctx); err != nil {
			d.Log.Warn("compacting snapshots", "err", err)
		} else if n > 0 {
			d.Log.Info("deleted duplicate snapshots", "count", n)
		}
		d.snapshotLoop(ctx)
	}()
	return nil
}

// camDiskSize is CamSize, or if that's 0 or doesn't fit, all the free space
// on backingfiles except MinFree, in whole GiB. The image is sparse, so an
// oversized one would only fail later, when the car fills it. Snapshots
// cost nothing until the car overwrites what they hold, and the snapshot
// loop keeps that within MinFree.
func (d *Drive) camDiskSize() (int64, error) {
	_, free, err := d.diskSpace(d.Backingfiles)
	if err != nil {
		if d.CamSize > 0 {
			return d.CamSize, nil
		}
		return 0, fmt.Errorf("sizing the cam disk: %w", err)
	}
	fits := (free - d.MinFree - d.extrasReserve()) / (1 << 30) * (1 << 30)
	if fits < 1<<30 {
		return 0, fmt.Errorf("only %d MiB free on %s; the cam disk needs 1 GiB plus %d MiB for snapshots and %d MiB for the other drives",
			free>>20, d.Backingfiles, d.MinFree>>20, d.extrasReserve()>>20)
	}
	switch {
	case d.CamSize == 0:
		return fits, nil
	case d.CamSize > fits:
		d.Log.Warn("cam_size doesn't fit with room for snapshots; using what does",
			"cam_size", d.CamSize, "using", fits)
		return fits, nil
	}
	return d.CamSize, nil
}

// prepareCamDisk creates the cam disk if it's missing, or repairs it. Only
// safe while the image isn't presented to the car.
func (d *Drive) prepareCamDisk(ctx context.Context) error {
	cam := d.CamDisk()
	_, err := os.Stat(cam)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		size, err := d.camDiskSize()
		if err != nil {
			return err
		}
		d.Log.Info("creating cam disk", "file", cam, "size", size)
		if err := d.createImage(ctx, d.Runner, cam, size, "CAM", "TeslaCam"); err != nil {
			return fmt.Errorf("creating cam disk: %w", err)
		}
	case err != nil:
		return err
	default:
		// The car cuts power without unmounting, so the image often has
		// errors. Fix them, and clean up, while the car can't see the drive.
		var c Cleanup
		m, err := d.maintainImage(ctx, d.Runner, cam, func(mnt string) error {
			var err error
			c, err = cleanCamDrive(mnt, d.KeepFreePercent, d.diskSpace, d.Archived)
			return err
		})
		if err != nil {
			// Present it anyway: a drive with errors beats no dashcam.
			d.Log.Warn("couldn't check and clean cam disk", "err", err)
			d.setError(err)
			break
		}
		if m.Repaired {
			d.Log.Info("repaired filesystem errors in cam disk")
		}
		d.Log.Info("cleaned cam disk", "junk_files", c.Junk, "deleted_archived_files", c.Deleted,
			"freed_mb", c.Freed>>20, "emptied_events", c.Events, "kept_not_archived", c.NotArchived,
			"free_mb", c.FreeAfter>>20, "total_mb", c.Total>>20, "trimmed", m.Trimmed)
		d.mu.Lock()
		d.lastCleanup = &c
		d.mu.Unlock()
	}
	return nil
}

// TakeSnapshot asks the snapshot loop to take one now, without waiting.
func (d *Drive) TakeSnapshot() {
	select {
	case d.snapshotSignal <- nil:
	default: // one is already pending
	}
}

// SnapshotNow takes a snapshot and waits for it. Snapshots are taken one at
// a time by the snapshot loop, so this queues behind one in progress.
func (d *Drive) SnapshotNow(ctx context.Context) error {
	done := make(chan error, 1)
	select {
	case d.snapshotSignal <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Drive) snapshotLoop(ctx context.Context) {
	timer := time.NewTimer(0) // first one right away, like at boot
	defer timer.Stop()
	space := time.NewTicker(d.SpaceCheck)
	defer space.Stop()
	for {
		var done chan error
		select {
		case <-ctx.Done():
			return
		case <-space.C:
			// Each snapshot keeps the blocks the car has overwritten since it
			// was taken. When that eats into MinFree, take a fresh one, which
			// holds nothing extra yet, so the older ones can go.
			if free, err := d.freeBytes(d.Backingfiles); err != nil || free >= d.MinFree/2 {
				continue
			} else {
				d.Log.Info("low on space, taking a snapshot early so older ones can go", "free", free)
			}
		case <-timer.C:
		case done = <-d.snapshotSignal:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		err := d.snapshotOnce(ctx)
		if done != nil {
			done <- err
		}
		timer.Reset(d.Interval)
	}
}

func (d *Drive) snapshotOnce(ctx context.Context) error {
	if _, err := d.Snapshots.Take(ctx); err != nil {
		d.Log.Error("snapshot failed", "err", err)
		d.setError(err)
		return err
	}
	d.setError(nil)
	if err := d.pruneSnapshots(ctx); err != nil {
		d.Log.Error("freeing space failed", "err", err)
		d.setError(err)
	}
	return nil
}

// pruneSnapshots deletes the oldest snapshots while free space is below
// MinFree, always keeping the newest one.
func (d *Drive) pruneSnapshots(ctx context.Context) error {
	for {
		free, err := d.freeBytes(d.Backingfiles)
		if err != nil {
			return err
		}
		if free >= d.MinFree {
			return nil
		}
		snaps, err := d.Snapshots.List()
		if err != nil {
			return err
		}
		if len(snaps) <= 1 {
			d.Log.Warn("low on space but only the newest snapshot is left", "free", free, "min_free", d.MinFree)
			return nil
		}
		d.Log.Info("low on space, deleting oldest snapshot", "id", snaps[0].ID, "free", free)
		if err := d.Snapshots.Release(ctx, snaps[0].ID); err != nil {
			return err
		}
	}
}

// Status reports the drive's current state.
func (d *Drive) Status() (Status, error) {
	st := Status{CamDisk: d.CamDisk()}
	if fi, err := os.Stat(st.CamDisk); err == nil {
		st.CamSize = fi.Size()
	}
	var err error
	if st.Gadget, err = d.Gadget.State(); err != nil {
		return st, err
	}
	snaps, err := d.Snapshots.List()
	if err != nil {
		return st, err
	}
	st.Snapshots = len(snaps)
	st.FreeBytes, _ = d.freeBytes(d.Backingfiles)
	d.mu.Lock()
	st.LastError = d.lastErr
	st.Cleanup = d.lastCleanup
	d.mu.Unlock()
	st.Extras = d.extrasStatus()
	d.mu.Lock()
	d.mu.Unlock()
	return st, nil
}

func (d *Drive) setError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		d.lastErr = ""
	} else {
		d.lastErr = err.Error()
	}
}
