package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lubaxe/glovebox/internal/drive"
	"github.com/lubaxe/glovebox/internal/gadget"
	"github.com/lubaxe/glovebox/internal/snapshot"
)

type fakeDrive struct{ requested int }

func (d *fakeDrive) Status() (drive.Status, error) {
	return drive.Status{CamDisk: "/backingfiles/cam_disk.bin", Gadget: gadget.State{Enabled: true, HostState: "configured"}, Snapshots: 1}, nil
}
func (d *fakeDrive) TakeSnapshot() { d.requested++ }

type fakeStore struct {
	snaps    []snapshot.Snapshot
	released []string
}

func (s *fakeStore) List() ([]snapshot.Snapshot, error) { return s.snaps, nil }
func (s *fakeStore) Release(_ context.Context, id string) error {
	s.released = append(s.released, id)
	return nil
}

func driveServer(t *testing.T) (*httptest.Server, *fakeDrive, *fakeStore) {
	t.Helper()
	mnt := t.TempDir()
	clip := filepath.Join(mnt, "TeslaCam", "SentryClips", "2026-10-15_12-00-00")
	if err := os.MkdirAll(clip, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clip, "2026-10-15_11-59-00-front.mp4"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &fakeDrive{}
	st := &fakeStore{snaps: []snapshot.Snapshot{{ID: "snap-000001", Mount: mnt}}}
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Drive: d, Snapshots: st}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, d, st
}

func do(t *testing.T, method, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestDriveStatus(t *testing.T) {
	ts, _, _ := driveServer(t)
	var st drive.Status
	resp := do(t, "GET", ts.URL+"/api/drive")
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Gadget.Connected() || st.Snapshots != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestTakeSnapshotRequest(t *testing.T) {
	ts, d, _ := driveServer(t)
	if resp := do(t, "POST", ts.URL+"/api/snapshots"); resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if d.requested != 1 {
		t.Errorf("requested = %d", d.requested)
	}
}

func TestSnapshotFiles(t *testing.T) {
	ts, _, _ := driveServer(t)
	resp := do(t, "GET", ts.URL+"/api/snapshots/snap-000001/files")
	var got struct {
		Files []fileEntry `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := "TeslaCam/SentryClips/2026-10-15_12-00-00/2026-10-15_11-59-00-front.mp4"
	if len(got.Files) != 1 || got.Files[0].Path != want || got.Files[0].Size != 1234 {
		t.Errorf("files = %+v", got.Files)
	}
	if resp := do(t, "GET", ts.URL+"/api/snapshots/snap-000099/files"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown snapshot: status = %d", resp.StatusCode)
	}
}

func TestReleaseSnapshot(t *testing.T) {
	ts, _, st := driveServer(t)
	if resp := do(t, "DELETE", ts.URL+"/api/snapshots/snap-000001"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if len(st.released) != 1 || st.released[0] != "snap-000001" {
		t.Errorf("released = %v", st.released)
	}
}

func TestDriveDisabled(t *testing.T) {
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp := do(t, "GET", ts.URL+"/api/drive"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}
