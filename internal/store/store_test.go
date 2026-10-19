package store

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/exfat/exfattest"
)

// clip returns fake MP4 data: an ftyp box, then tag repeated.
func clip(tag string, size int) []byte {
	b := append([]byte("\x00\x00\x00\x20ftyp"), bytes.Repeat([]byte(tag), size)...)
	return b[:size]
}

const clipSize = 150 << 10

type fixture struct {
	t     *testing.T
	s     *Store
	im    *exfattest.Image
	mount string // the "mounted snapshot": the same files, for classifying
	image string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "clips"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.cloneRange = func(dst, src *os.File, srcOff, length, dstOff int64) error {
		b := make([]byte, length)
		if _, err := src.ReadAt(b, srcOff); err != nil {
			return err
		}
		_, err := dst.WriteAt(b, dstOff)
		return err
	}
	// a clip's data says how the car moved: DRIV, PARK or NONE (no telemetry)
	s.Motion = func(_ context.Context, r io.ReaderAt, size int64) (Motion, error) {
		b := make([]byte, 4)
		_, err := r.ReadAt(b, 8)
		return map[string]Motion{"DRIV": Moving, "PARK": Parked}[string(b)], err
	}
	return &fixture{t: t, s: s, im: exfattest.New(2000), mount: t.TempDir(), image: filepath.Join(t.TempDir(), "snap.bin")}
}

func (f *fixture) add(rel string, data []byte, fragmented bool) {
	f.im.WriteFile(rel, data, fragmented)
	p := filepath.Join(f.mount, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) capture(id string) {
	f.t.Helper()
	if err := os.WriteFile(f.image, f.im.B, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Capture(context.Background(), id, f.image, f.mount); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) stored(rel string) []byte {
	b, err := os.ReadFile(filepath.Join(f.s.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	return b
}

const rc = "TeslaCam/RecentClips/"

func TestCaptureKeepsDrivingSkipsParked(t *testing.T) {
	f := newFixture(t)
	driveFront, driveBack := clip("DRIV", clipSize), clip("back", clipSize+5000)
	f.add(rc+"2026-10-19_07-40-00-front.mp4", driveFront, false)
	f.add(rc+"2026-10-19_07-40-00-back.mp4", driveBack, true) // fragmented
	f.add(rc+"2026-10-19_09-00-00-front.mp4", clip("PARK", clipSize), false)
	f.add(rc+"2026-10-19_09-00-00-back.mp4", clip("back", clipSize), false)
	f.add(rc+"2026-10-19_09-01-00-front.mp4", clip("DRIV", 1000), false) // too short to keep
	f.add("TeslaCam/SentryClips/2026-10-19_09-00-30/2026-10-19_09-00-00-front.mp4", clip("PARK", clipSize), false)
	f.capture("snap-000001")

	if !bytes.Equal(f.stored(rc+"2026-10-19_07-40-00-front.mp4"), driveFront) {
		t.Error("driving front clip not kept intact")
	}
	if !bytes.Equal(f.stored(rc+"2026-10-19_07-40-00-back.mp4"), driveBack) {
		t.Error("fragmented driving back clip not kept intact")
	}
	for _, rel := range []string{rc + "2026-10-19_09-00-00-front.mp4", rc + "2026-10-19_09-00-00-back.mp4",
		rc + "2026-10-19_09-01-00-front.mp4", "TeslaCam/SentryClips/2026-10-19_09-00-30/2026-10-19_09-00-00-front.mp4"} {
		if f.stored(rel) != nil {
			t.Errorf("%s kept", rel)
		}
	}
	if !f.s.Captured("snap-000001") || f.s.Captured("snap-000002") {
		t.Error("captured list wrong")
	}
	if f.s.Version() == 0 {
		t.Error("version not bumped")
	}
	if st := f.s.Status(); st.Minutes != 1 || st.Files != 2 {
		t.Errorf("status %+v", st)
	}
}

func TestCaptureNewerFirmwareDayFolders(t *testing.T) {
	f := newFixture(t)
	data := clip("DRIV", clipSize)
	f.add(rc+"2026-10-19/2026-10-19_07-40-00-front.mp4", data, false)
	f.capture("snap-000001")
	if !bytes.Equal(f.stored(rc+"2026-10-19/2026-10-19_07-40-00-front.mp4"), data) {
		t.Error("clip in a day folder not kept")
	}
}

func TestCaptureGrownClipAndSkipsKnownParked(t *testing.T) {
	f := newFixture(t)
	f.add(rc+"2026-10-19_09-00-00-front.mp4", clip("PARK", clipSize), false)
	f.capture("snap-000001")
	calls := 0
	motion := f.s.Motion
	f.s.Motion = func(ctx context.Context, r io.ReaderAt, size int64) (Motion, error) {
		calls++
		return motion(ctx, r, size)
	}
	f.capture("snap-000002")
	if calls != 0 {
		t.Errorf("a known parked clip was read again (%d times)", calls)
	}

	// a clip the car was still writing: kept short, then replaced in full
	f.add(rc+"2026-10-19_09-05-00-front.mp4", clip("DRIV", clipSize), false)
	f.capture("snap-000003")
	f.im.Delete(rc + "2026-10-19_09-05-00-front.mp4")
	full := clip("DRIV", clipSize+20000)
	f.add(rc+"2026-10-19_09-05-00-front.mp4", full, false)
	f.capture("snap-000004")
	if !bytes.Equal(f.stored(rc+"2026-10-19_09-05-00-front.mp4"), full) {
		t.Error("grown clip not replaced")
	}
}

func TestCaptureRejectsNonMP4(t *testing.T) {
	f := newFixture(t)
	f.add(rc+"2026-10-19_07-40-00-front.mp4", bytes.Repeat([]byte("junk"), clipSize/4), false)
	f.capture("snap-000001")
	if f.stored(rc+"2026-10-19_07-40-00-front.mp4") != nil {
		t.Error("kept data that isn't an MP4")
	}
	if left, _ := filepath.Glob(filepath.Join(f.s.Dir, "TeslaCam", "RecentClips", "*.part")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

func TestPruneArchivedFirstThenOldest(t *testing.T) {
	f := newFixture(t)
	f.s.now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }
	for _, m := range []string{"07-40-00", "07-41-00", "07-42-00", "17-00-00"} {
		f.add(rc+"2026-10-19_"+m+"-front.mp4", clip("DRIV", clipSize), false)
		f.add(rc+"2026-10-19_"+m+"-back.mp4", clip("back", clipSize), false)
	}
	f.capture("snap-000001")
	archived := func(rel string, _ int64) bool { return bytes.Contains([]byte(rel), []byte("07-42-00")) }

	// low on space until only want minutes are left
	want := 3
	low := func() bool { return f.s.Status().Minutes > want }
	// space for one minute: the archived one goes, though it's not the oldest
	dropped, err := f.s.Prune(low, archived)
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if f.stored(rc+"2026-10-19_07-42-00-front.mp4") != nil || f.stored(rc+"2026-10-19_07-40-00-front.mp4") == nil {
		t.Error("didn't delete the archived minute first")
	}

	// space for two more: the oldest unarchived ones go, and that's reported
	want = 1
	dropped, err = f.s.Prune(low, archived)
	if err != nil || dropped != 2 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if f.stored(rc+"2026-10-19_07-41-00-back.mp4") != nil || f.stored(rc+"2026-10-19_17-00-00-front.mp4") == nil {
		t.Error("didn't delete the oldest minutes")
	}
	if st := f.s.Status(); st.Dropped != 2 || st.Minutes != 1 {
		t.Errorf("status %+v", st)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	f := newFixture(t)
	f.add(rc+"2026-10-19_09-00-00-front.mp4", clip("PARK", clipSize), false)
	f.capture("snap-000001")
	if err := os.MkdirAll(filepath.Join(f.s.Dir, "TeslaCam"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.s.Dir, "TeslaCam", "x.mp4.part"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.s.Dir, f.s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Captured("snap-000001") || len(s.state.Parked) != 1 {
		t.Errorf("state lost: %+v", s.state)
	}
	if _, err := os.Stat(filepath.Join(f.s.Dir, "TeslaCam", "x.mp4.part")); err == nil {
		t.Error("interrupted clone left behind")
	}
	s.Forget("snap-000001")
	if s.Captured("snap-000001") {
		t.Error("Forget didn't")
	}
}

// The car records no telemetry while parked. Minutes without any are kept
// until a clip has shown the car records it at all, then left out.
func TestNoTelemetryCountsAsParkedOnceSeen(t *testing.T) {
	f := newFixture(t)
	f.add(rc+"2026-10-19_06-00-00-front.mp4", clip("NONE", clipSize), false)
	f.capture("snap-000001")
	if f.stored(rc+"2026-10-19_06-00-00-front.mp4") == nil {
		t.Error("dropped a clip without telemetry before knowing the car records it")
	}
	f.add(rc+"2026-10-19_07-40-00-front.mp4", clip("DRIV", clipSize), false)
	f.add(rc+"2026-10-19_09-00-00-front.mp4", clip("NONE", clipSize), false)
	f.capture("snap-000002")
	if f.stored(rc+"2026-10-19_09-00-00-front.mp4") != nil {
		t.Error("kept a parked minute without telemetry")
	}
	s, _ := Open(f.s.Dir, f.s.Log)
	if !s.state.Telemetry {
		t.Error("telemetry flag not saved")
	}
}

func TestIsParked(t *testing.T) {
	f := newFixture(t)
	f.add(rc+"2026-10-19_09-00-00-front.mp4", clip("PARK", clipSize), false)
	f.capture("snap-000001")
	if !f.s.IsParked(rc+"2026-10-19_09-00-00-front.mp4", clipSize) {
		t.Error("parked minute not reported")
	}
	if f.s.IsParked(rc+"2026-10-19_09-00-00-front.mp4", clipSize+1) {
		t.Error("a clip that grew since is still parked")
	}
}
