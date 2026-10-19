package web

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/clips"
	"github.com/lubaxe/glovebox/internal/snapshot"
)

func mp4Box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// teslaMP4 is a minimal MP4 with a ctts box, padded past the index's
// minimum clip size.
func teslaMP4() []byte {
	stbl := mp4Box("stbl", mp4Box("ctts", []byte("offsets")))
	moov := mp4Box("moov", mp4Box("trak", mp4Box("mdia", mp4Box("minf", stbl))))
	return bytes.Join([][]byte{mp4Box("ftyp", []byte("isom")), moov, mp4Box("mdat", make([]byte, 200<<10))}, nil)
}

const sentryDir = "TeslaCam/SentryClips/2026-10-15_00-40-00"

func clipServer(t *testing.T) *httptest.Server {
	t.Helper()
	mnt := t.TempDir()
	files := map[string][]byte{
		sentryDir + "/2026-10-15_00-39-00-front.mp4":        teslaMP4(),
		sentryDir + "/event.json":                           []byte(`{"timestamp":"2026-10-15T00:40:00","city":"Springfield","reason":"sentry_aware_object_detection"}`),
		"TeslaCam/RecentClips/2026-10-15_00-45-00-back.mp4": teslaMP4(),
		"TeslaCam/secret.txt":                               []byte("not a clip"),
	}
	for p, data := range files {
		full := filepath.Join(mnt, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{
		Version:   "test",
		Status:    fakeStatus{},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Drive:     &fakeDrive{},
		Snapshots: &fakeStore{snaps: []snapshot.Snapshot{{ID: "snap-000001", Mount: mnt}}},
		Clips: clips.NewLibrary(func() ([]clips.Snapshot, error) {
			return []clips.Snapshot{{ID: "snap-000001", Mount: mnt}}, nil
		}),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestEventsAPI(t *testing.T) {
	ts := clipServer(t)
	var events []clips.Summary
	if err := json.NewDecoder(do(t, "GET", ts.URL+"/api/events?kind=sentry").Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Info["city"] != "Springfield" {
		t.Fatalf("events = %+v", events)
	}
	var ev clips.Event
	if err := json.NewDecoder(do(t, "GET", ts.URL+"/api/events/sentry/"+events[0].ID).Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Clips) != 1 || ev.Clips[0].Cameras["front"].Snapshot != "snap-000001" {
		t.Errorf("event = %+v", ev)
	}
	if resp := do(t, "GET", ts.URL+"/api/events?kind=bogus"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad kind: status = %d", resp.StatusCode)
	}
	var recent []clips.Clip
	if err := json.NewDecoder(do(t, "GET", ts.URL+"/api/recent").Body).Decode(&recent); err != nil || len(recent) != 1 {
		t.Errorf("recent = %+v, %v", recent, err)
	}
}

func TestMediaPatchesMP4(t *testing.T) {
	ts := clipServer(t)
	resp := do(t, "GET", ts.URL+"/media/snap-000001/"+sentryDir+"/2026-10-15_00-39-00-front.mp4")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("status = %d, type = %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if bytes.Contains(body, []byte("ctts")) || !bytes.Contains(body, []byte("freeoffsets")) {
		t.Error("ctts box was not patched")
	}

	// browsers fetch video in ranges; the patch must apply there too
	req, _ := http.NewRequest("GET", ts.URL+"/media/snap-000001/"+sentryDir+"/2026-10-15_00-39-00-front.mp4", nil)
	off := bytes.Index(body, []byte("freeoffsets"))
	req.Header.Set("Range", "bytes="+strconv.Itoa(off)+"-"+strconv.Itoa(off+3))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	part, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != http.StatusPartialContent || string(part) != "free" {
		t.Errorf("range: status = %d, body = %q", r2.StatusCode, part)
	}
}

func TestMediaOnlyServesIndexedFiles(t *testing.T) {
	ts := clipServer(t)
	for _, p := range []string{
		"/media/snap-000001/TeslaCam/secret.txt",
		"/media/snap-000002/" + sentryDir + "/2026-10-15_00-39-00-front.mp4",
		"/media/snap-000001/TeslaCam/../../../etc/passwd",
		"/media/snap-000001/%2e%2e/%2e%2e/etc/passwd",
	} {
		resp := do(t, "GET", ts.URL+p)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: served", p)
		}
	}
}

func TestMediaDownload(t *testing.T) {
	ts := clipServer(t)
	resp := do(t, "GET", ts.URL+"/media/snap-000001/"+sentryDir+"/event.json?download=1")
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `attachment; filename=event.json`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestUI(t *testing.T) {
	ts := clipServer(t)
	resp := do(t, "GET", ts.URL+"/")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("<title>Glovebox</title>")) {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if resp := do(t, "GET", ts.URL+"/app.js"); resp.StatusCode != http.StatusOK {
		t.Errorf("app.js: status = %d", resp.StatusCode)
	}
}

func TestZipDownloads(t *testing.T) {
	ts := clipServer(t)
	read := func(url string) []string {
		t.Helper()
		resp := do(t, "GET", url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", url, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return names
	}
	names := read(ts.URL + "/api/events/sentry/2026-10-15_00-40-00/zip")
	if len(names) != 2 || names[0] != "sentry-2026-10-15_00-40-00/2026-10-15_00-39-00-front.mp4" || names[1] != "sentry-2026-10-15_00-40-00/event.json" {
		t.Errorf("event zip = %v", names)
	}
	names = read(ts.URL + "/api/recent/zip?from=2026-10-15T00:45:00&to=2026-10-15T00:45:00")
	if len(names) != 1 || names[0] != "drive-2026-10-15_00-45-00/2026-10-15_00-45-00-back.mp4" {
		t.Errorf("recent zip = %v", names)
	}
	if resp := do(t, "GET", ts.URL+"/api/recent/zip?from=2026-10-16T00:00:00&to=2026-10-16T01:00:00"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("empty range: status %d", resp.StatusCode)
	}
}

func TestMediaKeyframesNeedAnIndex(t *testing.T) {
	ts := clipServer(t)
	// the test clip has no sample tables: nothing to pick keyframes from
	resp := do(t, "GET", ts.URL+"/media/snap-000001/"+sentryDir+"/2026-10-15_00-39-00-front.mp4?keyframes=1")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
