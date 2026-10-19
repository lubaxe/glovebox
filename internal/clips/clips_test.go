package clips

import (
	"os"
	"path/filepath"
	"testing"
)

// drive builds a fake snapshot mount with the given files (path -> size).
func drive(t *testing.T, files map[string]int) Snapshot {
	t.Helper()
	mnt := t.TempDir()
	for p, size := range files {
		full := filepath.Join(mnt, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, size)
		if filepath.Base(p) == "event.json" {
			data = []byte(`{"timestamp":"2026-10-15T00:40:00","city":"Springfield","reason":"sentry_aware_object_detection","camera":"0","est_lat":"39.80"}`)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Snapshot{Mount: mnt}
}

const big = 200 << 10

func TestEventsAndRecent(t *testing.T) {
	s := drive(t, map[string]int{
		"TeslaCam/SentryClips/2026-10-15_00-40-00/2026-10-15_00-39-00-front.mp4": big,
		"TeslaCam/SentryClips/2026-10-15_00-40-00/2026-10-15_00-39-00-back.mp4":  big,
		"TeslaCam/SentryClips/2026-10-15_00-40-00/2026-10-15_00-40-00-front.mp4": big,
		"TeslaCam/SentryClips/2026-10-15_00-40-00/event.json":                    0,
		"TeslaCam/SentryClips/2026-10-15_00-40-00/thumb.png":                     10,
		"TeslaCam/SavedClips/2026-10-14_18-00-00/2026-10-14_17-59-00-front.mp4":  big,
		"TeslaCam/RecentClips/2026-10-15_00-44-00-front.mp4":                     big,
		"TeslaCam/RecentClips/2026-10-15/2026-10-15_00-45-00-front.mp4":          big,  // newer firmware layout
		"TeslaCam/RecentClips/2026-10-15_00-46-00-front.mp4":                     1000, // just started, too short
		"TeslaCam/RecentClips/notes.txt":                                         5,
		"System Volume Information/WPSettings.dat":                               12,
	})
	s.ID = "snap-000001"
	x := New()
	x.Refresh([]Snapshot{s})

	sentry := x.Events(Sentry)
	if len(sentry) != 1 {
		t.Fatalf("sentry = %+v", sentry)
	}
	ev := sentry[0]
	if ev.ID != "2026-10-15_00-40-00" || ev.Time != "2026-10-15T00:40:00" || ev.Clips != 2 {
		t.Errorf("summary = %+v", ev)
	}
	if ev.Info["city"] != "Springfield" || ev.Info["reason"] != "sentry_aware_object_detection" || ev.Thumb == nil {
		t.Errorf("info = %v, thumb = %v", ev.Info, ev.Thumb)
	}
	if len(ev.Cameras) != 2 || ev.Cameras[0] != "front" || ev.Cameras[1] != "back" {
		t.Errorf("cameras = %v", ev.Cameras)
	}

	full, ok := x.Event(Sentry, ev.ID, false)
	if !ok || len(full.Clips) != 2 || full.Clips[0].Start != "2026-10-15T00:39:00" || len(full.Clips[0].Cameras) != 2 {
		t.Errorf("event = %+v", full)
	}
	if saved := x.Events(Saved); len(saved) != 1 || saved[0].Info != nil {
		t.Errorf("saved = %+v", saved)
	}

	recent := x.Recent()
	if len(recent) != 2 || recent[0].Start != "2026-10-15T00:45:00" || recent[1].Start != "2026-10-15T00:44:00" {
		t.Errorf("recent = %+v", recent)
	}

	if _, ok := x.File("snap-000001", "TeslaCam/RecentClips/2026-10-15_00-44-00-front.mp4"); !ok {
		t.Error("indexed clip not found")
	}
	for _, p := range []string{"TeslaCam/RecentClips/notes.txt", "System Volume Information/WPSettings.dat", "../../etc/passwd"} {
		if _, ok := x.File("snap-000001", p); ok {
			t.Errorf("%s should not be servable", p)
		}
	}
}

func TestMostCompleteCopyWins(t *testing.T) {
	const clip = "TeslaCam/RecentClips/2026-10-15_00-45-00-front.mp4"
	mid := drive(t, map[string]int{clip: big})       // snapshot taken mid-write
	later := drive(t, map[string]int{clip: 4 * big}) // complete
	mid.ID, later.ID = "snap-000003", "snap-000004"

	x := New()
	x.Refresh([]Snapshot{mid, later})
	got := x.Recent()[0].Cameras["front"]
	if got.Snapshot != "snap-000004" || got.Size != 4*big {
		t.Errorf("chose %+v, want the complete copy", got)
	}

	// once the later snapshot is deleted, the partial copy is all there is
	x.Refresh([]Snapshot{mid})
	if got := x.Recent()[0].Cameras["front"]; got.Snapshot != "snap-000003" {
		t.Errorf("after delete chose %+v", got)
	}
}

func TestOlderSnapshotsKeepHistory(t *testing.T) {
	// the car deletes recent clips after an hour; older snapshots still have them
	old := drive(t, map[string]int{"TeslaCam/RecentClips/2026-10-14_09-00-00-front.mp4": big})
	now := drive(t, map[string]int{"TeslaCam/RecentClips/2026-10-15_09-00-00-front.mp4": big})
	old.ID, now.ID = "snap-000001", "snap-000002"
	x := New()
	x.Refresh([]Snapshot{old, now})
	if r := x.Recent(); len(r) != 2 {
		t.Errorf("recent = %+v, want clips from both snapshots", r)
	}
}

func TestSortCameras(t *testing.T) {
	got := SortCameras(map[string]bool{"right_pillar": true, "zz_new": true, "back": true, "front": true})
	want := []string{"front", "back", "right_pillar", "zz_new"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestEncryptedClipsAreFlagged(t *testing.T) {
	s := drive(t, map[string]int{
		"TeslaCam/EncryptedClips/-README_en.txt":                                               1500,
		"TeslaCam/EncryptedClips/RecentClips/2026-10-16_20-54-26-front.mp4":                    big,
		"TeslaCam/EncryptedClips/RecentClips/2026-10-16_20-54-26-back.mp4":                     big,
		"TeslaCam/EncryptedClips/RecentClips/2026-10-16_20-56-26-front.mp4":                    1000, // just started
		"TeslaCam/EncryptedClips/SentryClips/2026-10-16_21-00-00/2026-10-16_20-59-00-back.mp4": big,
		"TeslaCam/EncryptedClips/SentryClips/2026-10-16_21-00-00/event.json":                   0,
		"TeslaCam/SentryClips/2026-10-16_21-00-00/2026-10-16_20-59-00-front.mp4":               big, // same folder name, unencrypted
		"TeslaCam/RecentClips/2026-10-16_20-54-26-front.mp4":                                   big,
	})
	s.ID = "snap-000001"
	x := New()
	x.Refresh([]Snapshot{s})

	recent := x.Recent()
	if len(recent) != 2 || recent[0].Start != recent[1].Start || recent[0].Encrypted == recent[1].Encrypted {
		t.Fatalf("recent = %+v", recent)
	}
	for _, c := range recent {
		if c.Encrypted && len(c.Cameras) != 2 || !c.Encrypted && len(c.Cameras) != 1 {
			t.Errorf("clip %+v", c)
		}
	}
	sentry := x.Events(Sentry)
	if len(sentry) != 2 {
		t.Fatalf("sentry = %+v", sentry)
	}
	enc, ok := x.Event(Sentry, "2026-10-16_21-00-00", true)
	if !ok || !enc.Encrypted || enc.Meta == nil || len(enc.Clips) != 1 || enc.Clips[0].Cameras["back"].Path == "" {
		t.Errorf("encrypted event = %+v", enc)
	}
	if plain, ok := x.Event(Sentry, "2026-10-16_21-00-00", false); !ok || plain.Encrypted || plain.Clips[0].Cameras["front"].Path == "" {
		t.Errorf("plain event = %+v", plain)
	}
}

// The clip store changes under the same ID; a new Version rescans it.
func TestChangedVersionRescans(t *testing.T) {
	s := drive(t, map[string]int{"TeslaCam/RecentClips/2026-10-15_00-44-00-front.mp4": big})
	s.ID, s.Version = "store", 1
	x := New()
	x.Refresh([]Snapshot{s})
	p := filepath.Join(s.Mount, "TeslaCam", "RecentClips", "2026-10-15_00-45-00-front.mp4")
	if err := os.WriteFile(p, make([]byte, big), 0o644); err != nil {
		t.Fatal(err)
	}
	x.Refresh([]Snapshot{s})
	if n := len(x.Recent()); n != 1 {
		t.Fatalf("same version rescanned: %d clips", n)
	}
	s.Version = 2
	x.Refresh([]Snapshot{s})
	if n := len(x.Recent()); n != 2 {
		t.Fatalf("new version not rescanned: %d clips", n)
	}
	if f, ok := x.Locate("TeslaCam/RecentClips/2026-10-15_00-45-00-front.mp4"); !ok || f.Snapshot != "store" {
		t.Fatalf("Locate = %+v, %v", f, ok)
	}
}

// Parked minutes the clip store left out aren't listed as drives.
func TestHideRecent(t *testing.T) {
	s := drive(t, map[string]int{
		"TeslaCam/RecentClips/2026-10-15_00-44-00-front.mp4": big,
		"TeslaCam/RecentClips/2026-10-15_00-45-00-front.mp4": big,
	})
	s.ID = "snap-000001"
	x := New()
	x.HideRecent(func(p string, size int64) bool {
		return p == "TeslaCam/RecentClips/2026-10-15_00-45-00-front.mp4" && size == big
	})
	x.Refresh([]Snapshot{s})
	if r := x.Recent(); len(r) != 1 || r[0].Start != "2026-10-15T00:44:00" {
		t.Errorf("recent = %+v", r)
	}
}
