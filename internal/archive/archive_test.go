package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/clips"
)

// ---- fakes ----

type fakeTarget struct {
	mu        sync.Mutex
	reachable bool
	files     map[string]int64
	uploads   []string
	failAfter int // fail with a connection error after this many uploads (0 = never)
	onUpload  func(remote string)
}

func newTarget() *fakeTarget { return &fakeTarget{reachable: true, files: map[string]int64{}} }

func (t *fakeTarget) String() string { return "//nas/TeslaCam" }
func (t *fakeTarget) Reachable(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.reachable {
		return errors.New("no route to host")
	}
	return nil
}
func (t *fakeTarget) Connect(context.Context) (Session, error) { return t, nil }
func (t *fakeTarget) Close() error                             { return nil }
func (t *fakeTarget) Size(_ context.Context, remote string) (int64, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.files[remote]
	return n, ok, nil
}
func (t *fakeTarget) Upload(_ context.Context, remote string, r io.ReaderAt, size int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failAfter > 0 && len(t.uploads) >= t.failAfter {
		return errors.New("write tcp: connection reset by peer")
	}
	n, err := io.Copy(io.Discard, io.NewSectionReader(r, 0, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("size mismatch %d != %d", n, size)
	}
	t.files[remote] = n
	t.uploads = append(t.uploads, remote)
	if t.onUpload != nil {
		t.onUpload(remote)
	}
	return nil
}

type noSnap struct{ taken int }

func (s *noSnap) SnapshotNow(context.Context) error { s.taken++; return nil }

// ---- helpers ----

const clipSize = 200 << 10

type snapSet struct {
	mu    sync.Mutex
	snaps []clips.Snapshot
}

func (s *snapSet) list() ([]clips.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]clips.Snapshot(nil), s.snaps...), nil
}

func (s *snapSet) add(t *testing.T, id string, files map[string]int) {
	t.Helper()
	mnt := t.TempDir()
	for p, size := range files {
		full := filepath.Join(mnt, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, bytes.Repeat([]byte{'x'}, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.snaps = append(s.snaps, clips.Snapshot{ID: id, Mount: mnt})
	s.mu.Unlock()
}

func newArchiver(t *testing.T, snaps *snapSet) (*Archiver, *fakeTarget) {
	t.Helper()
	st, err := LoadState(filepath.Join(t.TempDir(), "archive-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := newTarget()
	a := New(target, clips.NewLibrary(snaps.list), &noSnap{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.FinalPassAfter = 0
	return a, target
}

var standardDrive = map[string]int{
	"TeslaCam/SentryClips/2026-10-14_09-00-00/2026-10-14_08-59-00-front.mp4": clipSize,
	"TeslaCam/SentryClips/2026-10-14_09-00-00/event.json":                    50,
	"TeslaCam/SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-front.mp4": clipSize,
	"TeslaCam/SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-back.mp4":  clipSize,
	"TeslaCam/SentryClips/2026-10-15_09-00-00/thumb.png":                     10,
	"TeslaCam/SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4":  clipSize,
	"TeslaCam/RecentClips/2026-10-15_13-00-00-front.mp4":                     clipSize,
}

// ---- tests ----

func TestRunUploadsInPriorityOrder(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)

	a.run(context.Background())

	want := []string{
		// newest Sentry event first: thumb, clips (front before back), metadata
		"SentryClips/2026-10-15_09-00-00/thumb.png",
		"SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-front.mp4",
		"SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-back.mp4",
		"SentryClips/2026-10-14_09-00-00/2026-10-14_08-59-00-front.mp4",
		"SentryClips/2026-10-14_09-00-00/event.json",
		"SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4",
	}
	if strings.Join(target.uploads, "\n") != strings.Join(want, "\n") {
		t.Errorf("uploads:\n%s\nwant:\n%s", strings.Join(target.uploads, "\n"), strings.Join(want, "\n"))
	}
	st := a.Status()
	if st.Last == nil || st.Last.Uploaded != 6 || len(st.Last.Events) != 3 || st.Archived != 6 {
		t.Errorf("status = %+v last = %+v", st, st.Last)
	}

	// nothing new: nothing uploaded
	target.uploads = nil
	a.run(context.Background())
	if len(target.uploads) != 0 {
		t.Errorf("second run uploaded %v", target.uploads)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, _ := newArchiver(t, snaps)
	a.run(context.Background())

	st, err := LoadState(a.State.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Count() != 6 || !st.Done("TeslaCam/SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4", clipSize) {
		t.Errorf("reloaded state has %d files", st.Count())
	}
}

func TestGrownFileIsUploadedAgain(t *testing.T) {
	const clip = "TeslaCam/SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-front.mp4"
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", map[string]int{clip: clipSize}) // caught mid-recording
	a, target := newArchiver(t, snaps)
	a.run(context.Background())

	snaps.add(t, "snap-000002", map[string]int{clip: 3 * clipSize}) // complete
	target.uploads = nil
	a.run(context.Background())
	if len(target.uploads) != 1 || target.files["SentryClips/2026-10-15_09-00-00/2026-10-15_08-59-00-front.mp4"] != 3*clipSize {
		t.Errorf("uploads = %v, files = %v", target.uploads, target.files)
	}
}

func TestConnectionLossStopsAndResumes(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)
	target.failAfter = 2

	a.run(context.Background())
	st := a.Status()
	if len(target.uploads) != 2 || st.Last == nil || st.Last.Failed != 1 || st.Last.Pending != 3 {
		t.Fatalf("uploads = %v, last = %+v", target.uploads, st.Last)
	}

	// back in range: picks up where it left off
	target.failAfter = 0
	target.uploads = nil
	a.run(context.Background())
	if len(target.uploads) != 4 {
		t.Errorf("resume uploaded %v", target.uploads)
	}
}

func TestExistingRemoteFileIsNotUploadedAgain(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)
	target.files["SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4"] = clipSize

	a.run(context.Background())
	for _, u := range target.uploads {
		if strings.HasPrefix(u, "SavedClips/") {
			t.Errorf("re-uploaded %s", u)
		}
	}
	if !a.State.Done("TeslaCam/SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4", clipSize) {
		t.Error("existing remote file should be recorded as archived")
	}
}

func TestRecentClipsOnlyWhenEnabled(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)
	a.Sentry, a.Saved, a.Recent = false, false, true
	a.run(context.Background())
	if len(target.uploads) != 1 || target.uploads[0] != "RecentClips/2026-10-15_13-00-00-front.mp4" {
		t.Errorf("uploads = %v", target.uploads)
	}
}

func TestRecentClipsRecordedAtHomeAreSkipped(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", map[string]int{
		"TeslaCam/RecentClips/2026-10-15_12-59-00-front.mp4": clipSize,
		"TeslaCam/RecentClips/2026-10-15_13-00-00-front.mp4": clipSize, // being recorded on arrival
	})
	a, target := newArchiver(t, snaps)
	a.Sentry, a.Saved, a.Recent = false, false, true
	a.run(context.Background())
	if len(target.uploads) != 2 || a.State.HomeCutoff() != "2026-10-15T13:01:00" {
		t.Fatalf("uploads = %v, cutoff = %q", target.uploads, a.State.HomeCutoff())
	}

	// the car stays awake in the garage and keeps recording
	snaps.add(t, "snap-000002", map[string]int{
		"TeslaCam/RecentClips/2026-10-15_13-01-00-front.mp4": clipSize, // pulling in
		"TeslaCam/RecentClips/2026-10-15_13-05-00-front.mp4": clipSize, // the garage
	})
	target.uploads = nil
	a.run(context.Background())
	if strings.Join(target.uploads, " ") != "RecentClips/2026-10-15_13-01-00-front.mp4" {
		t.Errorf("uploads = %v", target.uploads)
	}

	// a power cut while home doesn't move the cutoff
	st, err := LoadState(a.State.path)
	if err != nil || st.HomeCutoff() != "2026-10-15T13:01:00" {
		t.Fatalf("reloaded cutoff = %q, %v", st.HomeCutoff(), err)
	}

	// after the car left, the next arrival takes what was recorded since
	a.State.SetHomeCutoff("")
	target.uploads = nil
	a.run(context.Background())
	if strings.Join(target.uploads, " ") != "RecentClips/2026-10-15_13-05-00-front.mp4" {
		t.Errorf("after leaving: uploads = %v", target.uploads)
	}
}

func TestLoopArchivesWhenHome(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)
	a.HomeDelay, a.CheckInterval = 10*time.Millisecond, 10*time.Millisecond
	target.reachable = false
	var results []Result
	var mu sync.Mutex
	a.OnResult = func(r Result) { mu.Lock(); results = append(results, r); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	if st := a.Status(); st.State != Away || len(target.uploads) != 0 {
		t.Fatalf("while away: state = %s, uploads = %v", st.State, target.uploads)
	}
	target.mu.Lock()
	target.reachable = true
	target.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(results)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no archive run after coming home")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := a.Status(); st.State != Home {
		t.Errorf("after run: state = %s", st.State)
	}
	if results[0].Uploaded != 6 {
		t.Errorf("result = %+v", results[0])
	}
}

func TestReadCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte("# for glovebox\nusername=carcam\npassword=pa ss=word\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, pass, domain, err := ReadCredentials(path)
	if err != nil || user != "carcam" || pass != "pa ss=word" || domain != "" {
		t.Errorf("got %q %q %q %v", user, pass, domain, err)
	}
	if err := os.WriteFile(path, []byte("username=carcam\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadCredentials(path); err == nil {
		t.Error("missing password: want an error")
	}
}

func TestEncryptedClipsAreCopiedAsTheyAre(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", map[string]int{
		"TeslaCam/EncryptedClips/RecentClips/2026-10-16_20-54-26-front.mp4":                    clipSize,
		"TeslaCam/EncryptedClips/SentryClips/2026-10-16_21-00-00/2026-10-16_20-59-00-back.mp4": clipSize,
	})
	a, target := newArchiver(t, snaps)
	a.Recent = true
	a.run(context.Background())

	want := []string{
		"EncryptedClips/SentryClips/2026-10-16_21-00-00/2026-10-16_20-59-00-back.mp4",
		"EncryptedClips/RecentClips/2026-10-16_20-54-26-front.mp4",
	}
	if strings.Join(target.uploads, "\n") != strings.Join(want, "\n") {
		t.Errorf("uploads:\n%s", strings.Join(target.uploads, "\n"))
	}
	if st := a.Status(); st.Last == nil || len(st.Last.Events) != 1 {
		t.Errorf("last = %+v", st.Last)
	}
}

// Only the first run after getting home catches up on what the car recorded
// away; keep-awake goes by that.
func TestOnlyFirstRunAfterArrivingCatchesUp(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, target := newArchiver(t, snaps)
	a.HomeDelay, a.CheckInterval, a.RescanInterval = 10*time.Millisecond, 10*time.Millisecond, time.Hour
	var mu sync.Mutex
	seen := map[string]bool{} // remote -> CatchingUp during its upload
	target.onUpload = func(remote string) {
		mu.Lock()
		seen[remote] = a.Status().CatchingUp
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	waitFor := func(remote string) bool {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			v, ok := seen[remote]
			mu.Unlock()
			if ok {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s never uploaded", remote)
		return false
	}
	if !waitFor("SavedClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4") {
		t.Error("the first run after arriving should be catching up")
	}
	// recorded while parked, picked up by the next run once the catch-up
	// (with its final pass) is over
	for deadline := time.Now().Add(2 * time.Second); a.Status().CatchingUp; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still catching up")
		}
	}
	snaps.add(t, "snap-000002", map[string]int{"TeslaCam/SavedClips/2026-10-15_14-00-00/2026-10-15_13-59-00-front.mp4": clipSize})
	a.Trigger()
	if waitFor("SavedClips/2026-10-15_14-00-00/2026-10-15_13-59-00-front.mp4") {
		t.Error("later runs shouldn't keep the car awake")
	}
	if a.Status().CatchingUp {
		t.Error("still catching up after the runs")
	}
}

// What the car was still writing as it got home is picked up by the
// catch-up's final pass, while the car is still kept awake: a camera of the
// last minute that has grown, and the minute being recorded on arrival.
// Footage recorded after the home cutoff stays out.
func TestCatchUpFinalPass(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", map[string]int{
		"TeslaCam/RecentClips/2026-10-15_13-00-00-front.mp4": clipSize,
		"TeslaCam/RecentClips/2026-10-15_13-00-00-back.mp4":  clipSize / 2, // still being written
	})
	a, target := newArchiver(t, snaps)
	a.Recent = true
	a.HomeDelay, a.CheckInterval, a.RescanInterval = 10*time.Millisecond, 10*time.Millisecond, time.Hour
	a.FinalPassAfter = 50 * time.Millisecond
	var mu sync.Mutex
	seen := map[string]bool{} // remote -> CatchingUp during its upload
	var once sync.Once
	target.onUpload = func(remote string) {
		mu.Lock()
		seen[remote] = a.Status().CatchingUp
		mu.Unlock()
		// meanwhile the car finishes the last minute and the one after it
		once.Do(func() {
			snaps.add(t, "snap-000002", map[string]int{
				"TeslaCam/RecentClips/2026-10-15_13-00-00-front.mp4": clipSize,
				"TeslaCam/RecentClips/2026-10-15_13-00-00-back.mp4":  clipSize,
				"TeslaCam/RecentClips/2026-10-15_13-01-00-front.mp4": clipSize, // within the cutoff margin
				"TeslaCam/RecentClips/2026-10-15_13-05-00-front.mp4": clipSize, // parked at home
			})
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		_, ok := seen["RecentClips/2026-10-15_13-01-00-front.mp4"]
		mu.Unlock()
		if ok && !a.Status().CatchingUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final pass didn't happen; uploaded %v", seen)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["RecentClips/2026-10-15_13-01-00-front.mp4"] {
		t.Error("the final pass ran without keeping the car awake")
	}
	if got, _, _ := target.Size(context.Background(), "RecentClips/2026-10-15_13-00-00-back.mp4"); got != clipSize {
		t.Errorf("grown camera on the NAS is %d bytes, want %d", got, clipSize)
	}
	if _, ok := seen["RecentClips/2026-10-15_13-05-00-front.mp4"]; ok {
		t.Error("footage recorded at home was uploaded")
	}
}

// A power cut can leave the state file empty; archiving must carry on.
func TestEmptyStateFileStartsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive-state.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(path)
	if !errors.Is(err, ErrStateReset) || st == nil || st.Count() != 0 {
		t.Fatalf("state=%v err=%v", st, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("the unreadable file should be kept aside")
	}
	st.Mark("TeslaCam/x.mp4", 1)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadState(path); err != nil || again.Count() != 1 {
		t.Errorf("after saving: count=%d err=%v", again.Count(), err)
	}
}

func (t *fakeTarget) Remove(_ context.Context, remote string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.files, remote)
	return nil
}

func (t *fakeTarget) Mirror(context.Context, string, string) error { return nil }

func TestDrivesSyncOnArrivalAndTrigger(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, _ := newArchiver(t, snaps)
	a.HomeDelay, a.CheckInterval, a.RescanInterval = time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	var mu sync.Mutex
	syncs := 0
	a.SyncDrives = func(context.Context) { mu.Lock(); syncs++; mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return syncs }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d syncs, want %d", count(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(1)
	time.Sleep(100 * time.Millisecond) // several rescans
	if n := count(); n != 1 {
		t.Errorf("rescans synced the drives: %d syncs", n)
	}
	a.Trigger()
	wait(2)
}

func TestStateStartsOverForANewTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive-state.json")
	st, _ := LoadState(path)
	if st.UseTarget("smb://nas/TeslaCam/") {
		t.Error("a new state has nothing to start over")
	}
	st.Mark("TeslaCam/SentryClips/a/front.mp4", 10)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st, _ = LoadState(path)
	if st.UseTarget("smb://nas/TeslaCam/") || !st.Done("TeslaCam/SentryClips/a/front.mp4", 10) {
		t.Error("same target: the state should be kept")
	}
	if !st.UseTarget("rclone:gdrive:TeslaCam") || st.Done("TeslaCam/SentryClips/a/front.mp4", 10) {
		t.Error("new target: the state should start over")
	}

	// a state saved before targets were recorded is kept
	if err := os.WriteFile(path, []byte(`{"files":{"x":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ = LoadState(path)
	if st.UseTarget("smb://nas/TeslaCam/") || !st.Done("x", 1) {
		t.Error("an old state should be adopted")
	}
}

// cutTarget archives to a local directory. cut, while set, makes uploads
// stop partway like a power cut: the temporary file stays and nothing
// cleans it up. locked makes deleting files fail, like a file the server
// still holds open for a dead connection.
type cutTarget struct {
	root   string
	cut    bool
	locked bool
}

func (t *cutTarget) String() string                  { return t.root }
func (t *cutTarget) Reachable(context.Context) error { return nil }
func (t *cutTarget) Mirror(context.Context, string, string) error {
	return errors.New("not used")
}
func (t *cutTarget) Connect(context.Context) (Session, error) {
	return &cutSession{dirSession: &dirSession{root: t.root}, t: t}, nil
}

type cutSession struct {
	*dirSession
	t *cutTarget
}

func (s *cutSession) UploadVia(ctx context.Context, remote, tmp string, r io.ReaderAt, size int64) error {
	if s.t.cut {
		full := s.full(tmp)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte("half a clip"), 0o644); err != nil {
			return err
		}
		return errors.New("write tcp: connection reset by peer")
	}
	return s.dirSession.UploadVia(ctx, remote, tmp, r, size)
}

func (s *cutSession) Remove(ctx context.Context, remote string) error {
	if s.t.locked {
		return errors.New("STATUS_SHARING_VIOLATION")
	}
	return s.dirSession.Remove(ctx, remote)
}

func partialsIn(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".partial") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestUnfinishedUploadIsDeletedNextRun(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	st, err := LoadState(filepath.Join(t.TempDir(), "archive-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := &cutTarget{root: t.TempDir(), cut: true}
	a := New(target, clips.NewLibrary(snaps.list), &noSnap{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	a.run(context.Background())
	if got := partialsIn(t, target.root); len(got) != 1 {
		t.Fatalf("after the cut: %v", got)
	}
	// the record survives a restart (the cut was a power cut)
	reloaded, err := LoadState(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Partials(); len(got) != 1 || !strings.HasSuffix(got[0], ".partial") {
		t.Fatalf("recorded partials = %v", got)
	}
	a.State = reloaded

	target.cut = false
	a.run(context.Background())
	if got := partialsIn(t, target.root); len(got) != 0 {
		t.Errorf("left behind: %v", got)
	}
	if got := a.State.Partials(); len(got) != 0 {
		t.Errorf("still recorded: %v", got)
	}
	if a.State.Count() != 6 {
		t.Errorf("archived %d files, want 6", a.State.Count())
	}
}

func TestLockedPartialIsRetriedThenForgotten(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	st, err := LoadState(filepath.Join(t.TempDir(), "archive-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := &cutTarget{root: t.TempDir(), cut: true}
	a := New(target, clips.NewLibrary(snaps.list), &noSnap{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.run(context.Background())

	// still held open by the server: kept for later runs
	target.cut, target.locked = true, true
	for range maxPartialTries - 1 {
		a.run(context.Background())
	}
	// each run tried again and cut a new upload, so there are several now;
	// the first one has used up its tries
	before := len(a.State.Partials())
	a.run(context.Background())
	if after := len(a.State.Partials()); after != before {
		// one forgotten, one new from this run's cut upload
		t.Errorf("partials %d -> %d, want the same count (one given up, one added)", before, after)
	}

	// once the server lets go, the rest are deleted
	target.cut, target.locked = false, false
	a.run(context.Background())
	if got := a.State.Partials(); len(got) != 0 {
		t.Errorf("still recorded: %v", got)
	}
	if got := partialsIn(t, target.root); len(got) != 1 {
		t.Errorf("on the target: %v, want only the one given up on", got)
	}
}

// A snapshot released mid-run (for space, or once the clip store has its
// clips) mustn't fail the uploads queued from it: they come from the copy
// that's still there.
func TestSnapshotReleasedMidRun(t *testing.T) {
	files := map[string]int{
		"TeslaCam/RecentClips/2026-10-19_16-00-00-front.mp4": clipSize,
		"TeslaCam/RecentClips/2026-10-19_16-01-00-front.mp4": clipSize,
		"TeslaCam/RecentClips/2026-10-19_16-02-00-front.mp4": clipSize,
	}
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", files)
	a, target := newArchiver(t, snaps)
	a.Recent = true
	target.onUpload = func(string) {
		target.onUpload = nil
		snaps.mu.Lock()
		snaps.snaps = nil
		snaps.mu.Unlock()
		snaps.add(t, "store", files)
	}
	res := a.run(context.Background())
	if res.Failed != 0 || res.Uploaded != 3 {
		t.Errorf("uploaded %d, failed %d; want 3 and 0", res.Uploaded, res.Failed)
	}
}

func TestOutstanding(t *testing.T) {
	snaps := &snapSet{}
	snaps.add(t, "snap-000001", standardDrive)
	a, _ := newArchiver(t, snaps)
	if n, err := a.Outstanding(); err != nil || n != 6 { // Sentry and Saved; Recent is off
		t.Fatalf("before: %d, %v", n, err)
	}
	a.run(context.Background())
	if n, err := a.Outstanding(); err != nil || n != 0 {
		t.Errorf("after: %d, %v", n, err)
	}
}
