package snapshot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// fakeSystem pretends to be losetup, mount, umount, mountpoint and fsck.
type fakeSystem struct {
	t       *testing.T
	devDir  string
	mu      sync.Mutex
	nextDev int
	loops   map[string]string   // device -> file
	mounts  map[string][]string // mountpoint -> devices, stacked like the kernel does
	failOn  string              // command prefix that should fail
}

func newFakeSystem(t *testing.T) *fakeSystem {
	return &fakeSystem{t: t, devDir: t.TempDir(), loops: map[string]string{}, mounts: map[string][]string{}}
}

func (f *fakeSystem) run(cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && strings.HasPrefix(cmd, f.failOn) {
		return nil, &system.CmdError{Cmd: cmd, ExitCode: 32, Output: "simulated failure"}
	}
	args := strings.Fields(cmd)
	last := args[len(args)-1]
	switch {
	case strings.HasPrefix(cmd, "losetup --find"):
		dev := filepath.Join(f.devDir, fmt.Sprintf("loop%d", f.nextDev))
		f.nextDev++
		f.loops[dev] = last
		if err := os.WriteFile(dev+"p1", nil, 0o644); err != nil {
			f.t.Fatal(err)
		}
		return []byte(dev + "\n"), nil
	case strings.HasPrefix(cmd, "losetup --noheadings"):
		var out []string
		for dev, file := range f.loops {
			if file == last {
				out = append(out, dev)
			}
		}
		return []byte(strings.Join(out, "\n")), nil
	case strings.HasPrefix(cmd, "losetup --detach"):
		delete(f.loops, last)
	case strings.HasPrefix(cmd, "mount "):
		f.mounts[last] = append(f.mounts[last], args[len(args)-2])
	case strings.HasPrefix(cmd, "umount "):
		if stack := f.mounts[last]; len(stack) > 1 {
			f.mounts[last] = stack[:len(stack)-1]
		} else if len(stack) == 1 {
			delete(f.mounts, last)
		} else {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 32, Output: "not mounted"}
		}
	case strings.HasPrefix(cmd, "mountpoint "):
		if _, ok := f.mounts[last]; !ok {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 32}
		}
	case strings.HasPrefix(cmd, "fsck "):
	default:
		f.t.Errorf("unexpected command %q", cmd)
	}
	return nil, nil
}

func newManager(t *testing.T) (*Manager, *fakeSystem) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "cam_disk.bin")
	if err := os.WriteFile(source, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newFakeSystem(t)
	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	m := &Manager{
		Dir:       filepath.Join(dir, "snapshots"),
		Source:    source,
		MountRoot: filepath.Join(dir, "mnt"),
		Runner:    &system.FakeRunner{Handler: fs.run},
		Clone: func(src, dst string) error {
			b, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, b, 0o644)
		},
		Now: func() time.Time { clock = clock.Add(time.Minute); return clock },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// by default every snapshot has a file no other one has
	m.TOC = func(mnt string) (map[string]int64, error) {
		return map[string]int64{"TeslaCam/" + filepath.Base(mnt): 1}, nil
	}
	return m, fs
}

var sourceClock = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

// carWrites bumps the live image's modification time, as the car writing
// to the drive does.
func carWrites(t *testing.T, m *Manager) {
	t.Helper()
	sourceClock = sourceClock.Add(time.Minute)
	if err := os.Chtimes(m.Source, sourceClock, sourceClock); err != nil {
		t.Fatal(err)
	}
}

// withTOC makes the next snapshot contain exactly these files.
func withTOC(m *Manager, toc map[string]int64) {
	m.TOC = func(string) (map[string]int64, error) { return toc, nil }
}

func TestTakeAndList(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()

	s1, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	carWrites(t, m)
	s2, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != "snap-000001" || s2.ID != "snap-000002" {
		t.Errorf("ids = %s, %s", s1.ID, s2.ID)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Dir, s1.ID, "snap.bin")); string(b) != "disk" {
		t.Error("snapshot image is not a copy of the source")
	}
	if len(fs.mounts) != 2 {
		t.Errorf("mounts = %v, want 2", fs.mounts)
	}
	snaps, err := m.List()
	if err != nil || len(snaps) != 2 || !s1.Taken.Before(snaps[1].Taken) {
		t.Errorf("List = %+v, %v", snaps, err)
	}
}

func TestTakeCleansUpOnFailure(t *testing.T) {
	m, fs := newManager(t)
	fs.failOn = "mount "
	if _, err := m.Take(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "snap-000001")); !os.IsNotExist(err) {
		t.Error("failed snapshot should be deleted")
	}
	if len(fs.loops) != 0 {
		t.Errorf("loop devices left attached: %v", fs.loops)
	}
	if snaps, _ := m.List(); len(snaps) != 0 {
		t.Errorf("List = %+v", snaps)
	}
}

func TestRelease(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()
	s, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if len(fs.mounts) != 0 || len(fs.loops) != 0 {
		t.Errorf("mounts=%v loops=%v", fs.mounts, fs.loops)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, s.ID)); !os.IsNotExist(err) {
		t.Error("snapshot dir should be gone")
	}
	for _, bad := range []string{"../../etc", "snap-1", "snap-000009"} {
		if err := m.Release(ctx, bad); err == nil {
			t.Errorf("Release(%q) should fail", bad)
		}
	}
}

func TestRestoreAfterReboot(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()
	s, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// an interrupted snapshot: image but no meta.json
	if err := os.MkdirAll(filepath.Join(m.Dir, "snap-000002"), 0o755); err != nil {
		t.Fatal(err)
	}
	// reboot: mounts and loop devices are gone
	fs.mounts = map[string][]string{}
	fs.loops = map[string]string{}

	if err := m.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	carWrites(t, m)
	if _, ok := fs.mounts[s.Mount]; !ok {
		t.Errorf("snapshot not remounted: %v", fs.mounts)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "snap-000002")); !os.IsNotExist(err) {
		t.Error("incomplete snapshot should be deleted")
	}
	next, err := m.Take(ctx)
	if err != nil || next.ID != "snap-000002" {
		t.Errorf("next = %+v, %v", next, err)
	}
}

func TestUnchangedImageIsNotSnapshotted(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()
	s1, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := len(m.Runner.(*system.FakeRunner).Commands())
	s2, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Reused || s2.ID != s1.ID {
		t.Errorf("second take = %+v, want the first one reused", s2)
	}
	if after := len(m.Runner.(*system.FakeRunner).Commands()); after != before {
		t.Errorf("ran %d commands for an unchanged image", after-before)
	}
	if len(fs.loops) != 1 || len(fs.mounts) != 1 {
		t.Errorf("loops=%v mounts=%v", fs.loops, fs.mounts)
	}
}

func TestSnapshotWithNothingNewIsDiscarded(t *testing.T) {
	for name, next := range map[string]map[string]int64{
		"identical":     {"TeslaCam/a.mp4": 10, "TeslaCam/b.mp4": 20},
		"deletion only": {"TeslaCam/a.mp4": 10},
	} {
		t.Run(name, func(t *testing.T) {
			m, fs := newManager(t)
			ctx := context.Background()
			withTOC(m, map[string]int64{"TeslaCam/a.mp4": 10, "TeslaCam/b.mp4": 20})
			s1, err := m.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			carWrites(t, m)
			withTOC(m, next)
			s2, err := m.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !s2.Reused || s2.ID != s1.ID {
				t.Errorf("take = %+v, want %s reused", s2, s1.ID)
			}
			if _, err := os.Stat(filepath.Join(m.Dir, "snap-000002")); !os.IsNotExist(err) {
				t.Error("the discarded snapshot should be deleted")
			}
			if len(fs.loops) != 1 || len(fs.mounts) != 1 {
				t.Errorf("loops=%v mounts=%v", fs.loops, fs.mounts)
			}
			// the kept snapshot now counts as current: no work next time
			before := len(m.Runner.(*system.FakeRunner).Commands())
			if s3, _ := m.Take(ctx); !s3.Reused || len(m.Runner.(*system.FakeRunner).Commands()) != before {
				t.Error("an unchanged image should be skipped after a discarded snapshot")
			}
		})
	}
}

func TestSnapshotWithGrownFileIsKept(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	withTOC(m, map[string]int64{"TeslaCam/a.mp4": 10})
	if _, err := m.Take(ctx); err != nil {
		t.Fatal(err)
	}
	carWrites(t, m)
	withTOC(m, map[string]int64{"TeslaCam/a.mp4": 30}) // finished recording
	s2, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Reused || s2.ID != "snap-000002" {
		t.Errorf("take = %+v, want a new snapshot", s2)
	}
}

func TestCompact(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	for _, toc := range []map[string]int64{
		{"a": 1},         // 1: kept
		{"a": 1, "b": 1}, // 2: new file b
		{"a": 1, "b": 1}, // 3: duplicate of 2
		{"a": 1},         // 4: only a deletion
		{"a": 1, "c": 1}, // 5: new file c
	} {
		// write the snapshots as an older glovebox did: no deduplication
		id, _ := m.nextID()
		dir := filepath.Join(m.Dir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeTOC(dir, toc); err != nil {
			t.Fatal(err)
		}
		if err := writeMeta(dir, Snapshot{ID: id, Mount: filepath.Join(m.MountRoot, id)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := m.Compact(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Compact = %d, %v; want 2", n, err)
	}
	snaps, _ := m.List()
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	if fmt.Sprint(ids) != "[snap-000001 snap-000002 snap-000005]" {
		t.Errorf("left %v", ids)
	}
}

// Snapshots taken before the mount root changed still list where they're
// mounted now.
func TestListUsesCurrentMountRoot(t *testing.T) {
	m, _ := newManager(t)
	s, err := m.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Mount = "/run/oldname/snapshots/" + s.ID
	if err := writeMeta(filepath.Join(m.Dir, s.ID), s); err != nil {
		t.Fatal(err)
	}
	snaps, err := m.List()
	if err != nil || len(snaps) != 1 || snaps[0].Mount != filepath.Join(m.MountRoot, s.ID) {
		t.Errorf("snaps = %+v, %v", snaps, err)
	}
}

// Snapshots taken before the rename have the old mount root in meta.json.
// Restore must check where they're mounted now, or every restart of the
// service stacks another mount (and loop device) on top of the last.
func TestRestoreDoesNotStackMounts(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()
	s, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Mount = "/run/oldname/snapshots/" + s.ID
	if err := writeMeta(filepath.Join(m.Dir, s.ID), s); err != nil {
		t.Fatal(err)
	}
	for range 3 { // service restarts without a reboot
		if err := m.Restore(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mnt := filepath.Join(m.MountRoot, s.ID)
	if len(fs.mounts[mnt]) != 1 || len(fs.loops) != 1 {
		t.Errorf("mounts=%v loops=%v, want one of each", fs.mounts, fs.loops)
	}
}

// Releasing a snapshot that got mounted more than once removes every mount.
func TestReleaseUnmountsStackedMounts(t *testing.T) {
	m, fs := newManager(t)
	ctx := context.Background()
	s, err := m.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fs.mounts[s.Mount] = append(fs.mounts[s.Mount], "/dev/loop9p1", "/dev/loop10p1")
	if err := m.Release(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if len(fs.mounts) != 0 || len(fs.loops) != 0 {
		t.Errorf("mounts=%v loops=%v", fs.mounts, fs.loops)
	}
}
