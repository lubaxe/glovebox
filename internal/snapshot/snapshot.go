// Package snapshot takes copy-on-write snapshots of the car's disk image and
// mounts them read-only, so footage can be read while the car keeps writing
// to the live image.
package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/lubaxe/glovebox/internal/loopdev"
	"github.com/lubaxe/glovebox/internal/storage"
	"github.com/lubaxe/glovebox/internal/system"
)

const (
	imageName = "snap.bin"
	metaName  = "meta.json" // written last: a snapshot without it is incomplete
	tocName   = "toc.json"  // path -> size of every file, to compare snapshots
)

// maxStackedMounts bounds how many times release unmounts one snapshot.
const maxStackedMounts = 64

var idPattern = regexp.MustCompile(`^snap-(\d{6})$`)

// Snapshot is one point-in-time copy of the car's disk.
type Snapshot struct {
	ID       string    `json:"id"`
	Taken    time.Time `json:"taken"`
	Repaired bool      `json:"repaired"` // fsck fixed errors in the copy
	Mount    string    `json:"mount"`
	// SourceModified is the live image's modification time when the
	// snapshot was taken (or last confirmed current), so an unchanged image
	// isn't snapshotted again.
	SourceModified time.Time `json:"source_modified,omitzero"`
	// Reused is set by Take when nothing changed and the latest existing
	// snapshot was returned instead of a new one.
	Reused bool `json:"-"`
}

// Manager takes, mounts and releases snapshots.
type Manager struct {
	Dir       string // where snapshots are stored, on the reflink filesystem
	Source    string // the live disk image
	MountRoot string // where snapshots are mounted
	Runner    system.Runner
	Clone     func(src, dst string) error
	Now       func() time.Time
	Log       *slog.Logger
	// TOC lists the files under a mounted snapshot (path -> size).
	TOC func(mnt string) (map[string]int64, error)

	mu sync.Mutex
}

// NewManager returns a Manager for the standard layout under backingfiles.
func NewManager(backingfiles string, r system.Runner, log *slog.Logger) *Manager {
	return &Manager{
		Dir:       filepath.Join(backingfiles, "snapshots"),
		Source:    filepath.Join(backingfiles, "cam_disk.bin"),
		MountRoot: "/run/glovebox/snapshots",
		Runner:    r,
		Clone:     storage.Clone,
		Now:       time.Now,
		Log:       log,
		TOC:       walkTOC,
	}
}

// Take snapshots the live image, repairs the copy and mounts it read-only.
//
// It doesn't keep snapshots that add nothing: if the live image hasn't been
// written since the latest snapshot, or the new snapshot has no file that
// the latest one lacks (only deletions, or no change at all), the latest
// snapshot is returned with Reused set.
func (m *Manager) Take(ctx context.Context) (_ Snapshot, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fi, err := os.Stat(m.Source)
	if err != nil {
		return Snapshot{}, err
	}
	modified := fi.ModTime()
	snaps, err := m.list()
	if err != nil {
		return Snapshot{}, err
	}
	var prev *Snapshot
	if len(snaps) > 0 {
		prev = &snaps[len(snaps)-1]
		if prev.SourceModified.Equal(modified) {
			prev.Reused = true
			return *prev, nil
		}
	}

	id, err := m.nextID()
	if err != nil {
		return Snapshot{}, err
	}
	dir := filepath.Join(m.Dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Snapshot{}, err
	}
	defer func() {
		if err != nil {
			m.release(context.WithoutCancel(ctx), id)
		}
	}()

	taken := m.Now()
	image := filepath.Join(dir, imageName)
	if err := m.Clone(m.Source, image); err != nil {
		return Snapshot{}, fmt.Errorf("cloning %s: %w", m.Source, err)
	}

	// The copy is crash-consistent at best (the car may have been mid-write),
	// so repair it before mounting. This never touches the live image.
	dev, err := loopdev.Attach(ctx, m.Runner, image, false)
	if err != nil {
		return Snapshot{}, err
	}
	part, err := dev.WaitPartition(ctx, 1)
	if err != nil {
		return Snapshot{}, err
	}
	repaired, ferr := storage.Fsck(ctx, m.Runner, part)
	if ferr != nil {
		m.Log.Warn("fsck couldn't fully repair snapshot, mounting anyway", "id", id, "err", ferr)
	}
	mnt, err := m.mount(ctx, id, part)
	if err != nil {
		return Snapshot{}, err
	}

	toc, tocErr := m.TOC(mnt)
	if tocErr != nil {
		m.Log.Warn("couldn't list snapshot files", "id", id, "err", tocErr)
	} else {
		if prev != nil {
			if prevTOC, err := m.tocOf(*prev); err == nil && !hasNew(toc, prevTOC) {
				// nothing the previous snapshot doesn't already have
				if err := m.release(ctx, id); err != nil {
					return Snapshot{}, err
				}
				prev.SourceModified = modified
				if err := writeMeta(filepath.Join(m.Dir, prev.ID), *prev); err != nil {
					return Snapshot{}, err
				}
				m.Log.Debug("nothing new on the drive, keeping the latest snapshot", "id", prev.ID)
				prev.Reused = true
				return *prev, nil
			}
		}
		if err := writeTOC(dir, toc); err != nil {
			return Snapshot{}, err
		}
	}

	snap := Snapshot{ID: id, Taken: taken, Repaired: repaired, Mount: mnt, SourceModified: modified}
	if err := writeMeta(dir, snap); err != nil {
		return Snapshot{}, err
	}
	m.Log.Info("took snapshot", "id", id, "repaired", repaired)
	return snap, nil
}

// Compact deletes snapshots that have nothing the snapshot before them
// lacks, for example ones taken before Take learned to skip them. It keeps
// the oldest of each run of identical snapshots, since that one has
// everything the others have. It returns how many it deleted.
func (m *Manager) Compact(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snaps, err := m.list()
	if err != nil || len(snaps) < 2 {
		return 0, err
	}
	kept, err := m.tocOf(snaps[0])
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, s := range snaps[1:] {
		toc, err := m.tocOf(s)
		if err != nil {
			m.Log.Warn("couldn't list snapshot files, keeping it", "id", s.ID, "err", err)
			continue
		}
		if hasNew(toc, kept) {
			kept = toc
			continue
		}
		if err := m.release(ctx, s.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// tocOf returns a snapshot's file list, from its toc.json or, for snapshots
// taken before those existed, by listing the mounted files (and saving it).
func (m *Manager) tocOf(s Snapshot) (map[string]int64, error) {
	dir := filepath.Join(m.Dir, s.ID)
	if b, err := os.ReadFile(filepath.Join(dir, tocName)); err == nil {
		var toc map[string]int64
		if err := json.Unmarshal(b, &toc); err == nil {
			return toc, nil
		}
	}
	toc, err := m.TOC(s.Mount)
	if err != nil {
		return nil, err
	}
	_ = writeTOC(dir, toc)
	return toc, nil
}

// hasNew reports whether toc has a file that old lacks or has at another size.
func hasNew(toc, old map[string]int64) bool {
	for p, size := range toc {
		if oldSize, ok := old[p]; !ok || oldSize != size {
			return true
		}
	}
	return false
}

// walkTOC lists every file under mnt with its size.
func walkTOC(mnt string) (map[string]int64, error) {
	if _, err := os.Stat(mnt); err != nil {
		return nil, err
	}
	toc := map[string]int64{}
	err := filepath.WalkDir(mnt, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable parts of a damaged snapshot
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				rel, _ := filepath.Rel(mnt, p)
				toc[filepath.ToSlash(rel)] = info.Size()
			}
		}
		return nil
	})
	return toc, err
}

func writeTOC(dir string, toc map[string]int64) error {
	b, err := json.Marshal(toc)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, tocName), b, 0o644)
}

// List returns the complete snapshots, oldest first.
func (m *Manager) List() ([]Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list()
}

// Release unmounts and deletes a snapshot.
func (m *Manager) Release(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid snapshot id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(m.Dir, id)); err != nil {
		return err
	}
	return m.release(ctx, id)
}

// Restore is called at startup: it deletes snapshots that were interrupted
// and remounts the complete ones (mounts don't survive a reboot).
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		id := e.Name()
		if !idPattern.MatchString(id) {
			continue
		}
		snap, err := readMeta(filepath.Join(m.Dir, id))
		if err != nil {
			m.Log.Warn("removing incomplete snapshot", "id", id, "err", err)
			if err := m.release(ctx, id); err != nil {
				return err
			}
			continue
		}
		// Not snap.Mount: snapshots taken before the rename recorded the old
		// mount root, which is never mounted, so they'd be mounted again on
		// every restart, stacking mounts and loop devices.
		if m.mounted(ctx, filepath.Join(m.MountRoot, snap.ID)) {
			continue
		}
		dev, err := loopdev.Attach(ctx, m.Runner, filepath.Join(m.Dir, id, imageName), true)
		if err != nil {
			return err
		}
		part, err := dev.WaitPartition(ctx, 1)
		if err == nil {
			_, err = m.mount(ctx, id, part)
		}
		if err != nil {
			_ = dev.Detach(ctx)
			m.Log.Warn("couldn't remount snapshot", "id", id, "err", err)
		}
	}
	return nil
}

func (m *Manager) mount(ctx context.Context, id, part string) (string, error) {
	mnt := filepath.Join(m.MountRoot, id)
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}
	if _, err := m.Runner.Run(ctx, "mount", "-o", "ro,nosuid,nodev,noexec", part, mnt); err != nil {
		return "", fmt.Errorf("mounting snapshot %s: %w", id, err)
	}
	return mnt, nil
}

func (m *Manager) mounted(ctx context.Context, path string) bool {
	_, err := m.Runner.Run(ctx, "mountpoint", "-q", path)
	return err == nil
}

// release undoes everything Take does, tolerating parts that don't exist.
func (m *Manager) release(ctx context.Context, id string) error {
	mnt := filepath.Join(m.MountRoot, id)
	// A loop, since it may be mounted more than once (Restore used to do
	// that). Lazy, so a clip being read (archived or played) can't keep the
	// space from being freed; it goes once the reader is done.
	for i := 0; m.mounted(ctx, mnt); i++ {
		if i == maxStackedMounts {
			return fmt.Errorf("snapshot %s is still mounted after %d unmounts", id, i)
		}
		if _, err := m.Runner.Run(ctx, "umount", "--lazy", mnt); err != nil {
			return fmt.Errorf("unmounting snapshot %s: %w", id, err)
		}
	}
	_ = os.Remove(mnt)
	image := filepath.Join(m.Dir, id, imageName)
	if _, err := os.Stat(image); err == nil {
		devs, err := loopdev.AttachedTo(ctx, m.Runner, image)
		if err != nil {
			return err
		}
		for _, d := range devs {
			if err := d.Detach(ctx); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(m.Dir, id)); err != nil {
		return err
	}
	m.Log.Info("released snapshot", "id", id)
	return nil
}

func (m *Manager) list() ([]Snapshot, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	for _, e := range entries {
		if !idPattern.MatchString(e.Name()) {
			continue
		}
		if snap, err := readMeta(filepath.Join(m.Dir, e.Name())); err == nil {
			// Where it's mounted now, not where it was when taken: the mount
			// root can change (it did when the project was renamed).
			snap.Mount = filepath.Join(m.MountRoot, snap.ID)
			snaps = append(snaps, snap)
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	return snaps, nil
}

// nextID returns the id after the highest existing one, complete or not.
func (m *Manager) nextID() (string, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	next := 1
	for _, e := range entries {
		if match := idPattern.FindStringSubmatch(e.Name()); match != nil {
			if n, _ := strconv.Atoi(match[1]); n >= next {
				next = n + 1
			}
		}
	}
	if next > 999999 {
		return "", errors.New("snapshot ids exhausted")
	}
	return fmt.Sprintf("snap-%06d", next), nil
}

func writeMeta(dir string, snap Snapshot) error {
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, metaName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, metaName))
}

func readMeta(dir string) (Snapshot, error) {
	var snap Snapshot
	b, err := os.ReadFile(filepath.Join(dir, metaName))
	if err != nil {
		return snap, err
	}
	return snap, json.Unmarshal(b, &snap)
}
