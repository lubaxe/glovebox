package drive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lubaxe/glovebox/internal/snapshot"
	"github.com/lubaxe/glovebox/internal/store"
	"github.com/lubaxe/glovebox/internal/system"
)

func writeSized(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

type usageStore struct {
	fakeStore
	usage store.Usage
}

func (s *usageStore) Usage(func(string, int64) bool) store.Usage { return s.usage }

func TestStorage(t *testing.T) {
	d, _, s, _ := newDrive(t)
	mnt := t.TempDir() // the newest snapshot
	for _, m := range []string{"16-00-00", "16-01-00"} {
		writeSized(t, filepath.Join(mnt, "TeslaCam", "RecentClips", "2026-10-19_"+m+"-front.mp4"), 3000)
		writeSized(t, filepath.Join(mnt, "TeslaCam", "RecentClips", "2026-10-19_"+m+"-back.mp4"), 1000)
	}
	writeSized(t, filepath.Join(mnt, "TeslaCam", "SentryClips", "2026-10-19_09-00-00", "2026-10-19_08-59-00-front.mp4"), 500)
	writeSized(t, filepath.Join(mnt, "TeslaCam", "EncryptedClips", "SavedClips", "2026-10-19_10-00-00", "2026-10-19_09-59-00-front.mp4"), 200)
	s.snaps = []snapshot.Snapshot{{ID: "snap-000001", Mount: mnt}}
	if err := os.WriteFile(d.CamDisk(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(d.CamDisk(), 52<<30); err != nil {
		t.Fatal(err)
	}
	d.allocated = func(string) (int64, error) { return 50 << 30, nil }
	d.diskSpace = func(p string) (int64, int64, error) {
		if p == mnt {
			return 20000, 10000, nil // the car's file system
		}
		return 58 << 30, 4 << 30, nil
	}
	d.MinFree = 4 << 30
	d.Store = &usageStore{usage: store.Usage{Bytes: 30 << 30, Minutes: 120, ArchivedBytes: 10 << 30, ArchivedMinutes: 40}}

	st, err := d.Storage()
	if err != nil {
		t.Fatal(err)
	}
	c := st.Car
	if !c.Known || c.Size != 52<<30 || c.Recent != 8000 || c.RecentMinutes != 2 || c.Sentry != 500 || c.Saved != 200 ||
		c.Free != 10000 || c.Other != 20000-10000-8700 {
		t.Errorf("car = %+v", c)
	}
	if st.Outside != (58-4-50)<<30 {
		t.Errorf("outside = %d GiB", st.Outside>>30)
	}
	if !st.RateMeasured || st.Rate != 15<<30 { // 30 GiB in 120 minutes
		t.Errorf("rate = %d MiB/h, measured %v", st.Rate>>20, st.RateMeasured)
	}
	// the car's hour (15 GiB), its events with room to grow, 2 GiB slack: rounded up
	if st.MinCarSize != 18<<30 {
		t.Errorf("min car size = %d GiB", st.MinCarSize>>30)
	}
	if st.MaxCarSize != (58-4-30)<<30 {
		t.Errorf("max car size = %d GiB", st.MaxCarSize>>30)
	}
}

func TestRecreateCamDisk(t *testing.T) {
	d, g, s, c := newDrive(t)
	if err := os.WriteFile(d.CamDisk(), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.state.Enabled, g.state.Files = true, []string{d.CamDisk()} // presented, as after a restart
	s.snaps = []snapshot.Snapshot{{ID: "snap-000001"}, {ID: "snap-000002"}}
	st := &fakeStore{captured: map[string]bool{"snap-000002": true}}
	d.Store = st
	d.diskSpace = func(string) (int64, int64, error) { return 58 << 30, 40 << 30, nil }

	if err := d.RequestRecreate(20<<30 + 1); !errors.Is(err, ErrBadSize) {
		t.Errorf("odd size: %v", err)
	}
	if err := d.RequestRecreate(200 << 30); !errors.Is(err, ErrBadSize) {
		t.Errorf("too big: %v", err)
	}
	if err := d.RequestRecreate(32 << 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Storage(); got.Recreate != 32<<30 {
		t.Errorf("pending recreate = %d", got.Recreate)
	}

	var size int64
	d.createImage = func(_ context.Context, _ system.Runner, path string, n int64, _, _ string) error {
		size = n
		c.created = append(c.created, path)
		return os.WriteFile(path, nil, 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if g.disabled != 1 || len(g.enabled) != 1 {
		t.Errorf("disabled %d, enabled %d times", g.disabled, len(g.enabled))
	}
	if len(c.created) != 1 || size != 32<<30 {
		t.Errorf("created %v at %d GiB", c.created, size>>30)
	}
	if b, _ := os.ReadFile(d.CamDisk()); string(b) == "old" {
		t.Error("old drive kept")
	}
	if len(s.released) < 2 || st.captured["snap-000002"] {
		t.Errorf("snapshots released %v, store still lists %v", s.released, st.captured)
	}
	if _, err := os.Stat(d.recreateFile()); err == nil {
		t.Error("request left behind")
	}
}

func TestCancelRecreate(t *testing.T) {
	d, _, _, _ := newDrive(t)
	if err := d.CancelRecreate(); err != nil {
		t.Errorf("cancel without a request: %v", err)
	}
	if err := d.RequestRecreate(32 << 30); err != nil {
		t.Fatal(err)
	}
	if err := d.CancelRecreate(); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Storage(); st.Recreate != 0 {
		t.Error("still pending")
	}
}
