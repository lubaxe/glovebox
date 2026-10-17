package drive

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/storage"
	"github.com/lubaxe/glovebox/internal/system"
)

type fakeGadget struct {
	state   gadget.State
	enabled [][]gadget.LUN
}

func (g *fakeGadget) Enable(_ context.Context, luns []gadget.LUN) error {
	g.enabled = append(g.enabled, luns)
	g.state.Enabled = true
	return nil
}

func (g *fakeGadget) State() (gadget.State, error) { return g.state, nil }

type fakeSnaps struct {
	mu       sync.Mutex
	snaps    []snapshot.Snapshot
	taken    int
	released []string
	restored bool
}

func (s *fakeSnaps) Take(context.Context) (snapshot.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken++
	snap := snapshot.Snapshot{ID: "snap-00000" + string(rune('0'+s.taken))}
	s.snaps = append(s.snaps, snap)
	return snap, nil
}

func (s *fakeSnaps) List() ([]snapshot.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]snapshot.Snapshot(nil), s.snaps...), nil
}

func (s *fakeSnaps) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, id)
	s.snaps = s.snaps[1:]
	return nil
}

func (s *fakeSnaps) Restore(context.Context) error { s.restored = true; return nil }

func (s *fakeSnaps) Compact(context.Context) (int, error) { return 0, nil }

type calls struct {
	created  []string
	repaired []string
}

func newDrive(t *testing.T) (*Drive, *fakeGadget, *fakeSnaps, *calls) {
	t.Helper()
	g := &fakeGadget{}
	s := &fakeSnaps{}
	c := &calls{}
	d := New(t.TempDir(), 8<<30, time.Hour, 1<<30, g, s, &system.FakeRunner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.checkReflink = func(string) error { return nil }
	d.createImage = func(_ context.Context, _ system.Runner, path string, _ int64, _ string) error {
		c.created = append(c.created, path)
		return os.WriteFile(path, nil, 0o644)
	}
	mounted := t.TempDir() // stands in for the mounted cam drive
	d.maintainImage = func(_ context.Context, _ system.Runner, path string, work func(string) error) (storage.Maintenance, error) {
		c.repaired = append(c.repaired, path)
		return storage.Maintenance{Repaired: true}, work(mounted)
	}
	d.diskSpace = func(string) (int64, int64, error) { return 64 << 30, 60 << 30, nil }
	d.freeBytes = func(string) (int64, error) { return 5 << 30, nil }
	return d, g, s, c
}

func TestStartCreatesMissingCamDisk(t *testing.T) {
	d, g, s, c := newDrive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.created) != 1 || c.created[0] != filepath.Join(d.Backingfiles, "cam_disk.bin") {
		t.Errorf("created = %v", c.created)
	}
	if len(c.repaired) != 0 {
		t.Error("a new disk doesn't need repairing")
	}
	if !s.restored || len(g.enabled) != 1 || g.enabled[0][0].File != d.CamDisk() {
		t.Errorf("restored=%v enabled=%v", s.restored, g.enabled)
	}
}

func TestStartRepairsExistingCamDisk(t *testing.T) {
	d, g, _, c := newDrive(t)
	if err := os.WriteFile(d.CamDisk(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.repaired) != 1 || len(c.created) != 0 || len(g.enabled) != 1 {
		t.Errorf("repaired=%v created=%v enabled=%v", c.repaired, c.created, g.enabled)
	}
}

func TestStartLeavesConnectedDriveAlone(t *testing.T) {
	d, g, s, c := newDrive(t)
	if err := os.WriteFile(d.CamDisk(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	g.state = gadget.State{Enabled: true, HostState: "configured"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.repaired) != 0 || len(c.created) != 0 || len(g.enabled) != 0 {
		t.Errorf("touched the live drive: repaired=%v created=%v enabled=%v", c.repaired, c.created, g.enabled)
	}
	if !s.restored {
		t.Error("snapshots should still be restored")
	}
}

func TestStartNeedsReflink(t *testing.T) {
	d, g, _, c := newDrive(t)
	d.checkReflink = func(string) error { return errors.New("ext4") }
	if err := d.Start(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if len(c.created) != 0 || len(g.enabled) != 0 {
		t.Error("should stop before touching anything")
	}
}

func TestSnapshotLoopTakesFirstSnapshotRightAway(t *testing.T) {
	d, _, s, _ := newDrive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if snaps, _ := s.List(); len(snaps) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot taken")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.TakeSnapshot()
	for {
		if snaps, _ := s.List(); len(snaps) > 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("TakeSnapshot didn't trigger a snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPruneDeletesOldestUntilEnoughSpace(t *testing.T) {
	d, _, s, _ := newDrive(t)
	s.snaps = []snapshot.Snapshot{{ID: "snap-000001"}, {ID: "snap-000002"}, {ID: "snap-000003"}}
	free := int64(0)
	d.freeBytes = func(string) (int64, error) { return free, nil }
	d.MinFree = 2
	// each release frees one byte
	d.Snapshots = &countingSnaps{fakeSnaps: s, onRelease: func() { free++ }}
	if err := d.pruneSnapshots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.released) != 2 || s.released[0] != "snap-000001" || s.released[1] != "snap-000002" {
		t.Errorf("released = %v", s.released)
	}
}

func TestPruneKeepsNewestSnapshot(t *testing.T) {
	d, _, s, _ := newDrive(t)
	s.snaps = []snapshot.Snapshot{{ID: "snap-000001"}}
	d.freeBytes = func(string) (int64, error) { return 0, nil }
	if err := d.pruneSnapshots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.released) != 0 {
		t.Errorf("released = %v", s.released)
	}
}

type countingSnaps struct {
	*fakeSnaps
	onRelease func()
}

func (c *countingSnaps) Release(ctx context.Context, id string) error {
	c.onRelease()
	return c.fakeSnaps.Release(ctx, id)
}

func TestSnapshotNowWaits(t *testing.T) {
	d, _, s, _ := newDrive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := s.List()
	if err := d.SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.List()
	if len(after) <= len(before) {
		t.Errorf("snapshots before=%d after=%d, want a new one", len(before), len(after))
	}
}

func TestCamDiskSize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		camSize, want int64
	}{
		{"as large as fits", 0, 26 << 30},     // 30.5 GiB free - 4 GiB, whole GiB
		{"capped to fit", 40 << 30, 26 << 30}, // a small card
		{"as configured", 8 << 30, 8 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, _ := newDrive(t)
			var gotSize int64
			d.createImage = func(_ context.Context, _ system.Runner, path string, size int64, _ string) error {
				gotSize = size
				return os.WriteFile(path, nil, 0o644)
			}
			d.CamSize, d.MinFree = tc.camSize, 4<<30
			d.diskSpace = func(string) (int64, int64, error) { return 32 << 30, 61 << 29, nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if gotSize != tc.want {
				t.Errorf("cam disk size = %d MiB, want %d", gotSize>>20, tc.want>>20)
			}
		})
	}
}

func TestCamDiskDoesntFit(t *testing.T) {
	d, _, _, _ := newDrive(t)
	d.MinFree = 4 << 30
	d.diskSpace = func(string) (int64, int64, error) { return 5 << 30, 4 << 30, nil }
	if err := d.Start(context.Background()); err == nil {
		t.Fatal("want an error")
	}
}

func TestLowSpaceTakesSnapshotEarly(t *testing.T) {
	d, _, s, _ := newDrive(t)
	d.Interval = time.Hour
	d.SpaceCheck = 10 * time.Millisecond
	free := atomic.Int64{}
	free.Store(5 << 30)
	d.freeBytes = func(string) (int64, error) { return free.Load(), nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	taken := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.taken
	}
	waitSnaps := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if taken() >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("want %d snapshots taken", n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitSnaps(1) // the one at startup
	time.Sleep(50 * time.Millisecond)
	if n := taken(); n != 1 {
		t.Fatalf("plenty of space, but %d snapshots taken", n)
	}
	free.Store(100 << 20) // the car overwrote a lot
	waitSnaps(2)
}
