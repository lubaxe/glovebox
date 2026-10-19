package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/drive"
)

type fakeSizing struct {
	st      drive.Storage
	resized int64
}

func (f *fakeSizing) Storage() (drive.Storage, error) { return f.st, nil }
func (f *fakeSizing) Resize(_ context.Context, size int64) error {
	if size < f.st.MinCarSize || size > f.st.MaxCarSize {
		return fmt.Errorf("%w: no", drive.ErrBadSize)
	}
	f.resized, f.st.Recreate = size, size
	return nil
}
func (f *fakeSizing) CancelResize() error { f.st.Recreate = 0; return nil }

func TestStorageResize(t *testing.T) {
	z := &fakeSizing{st: drive.Storage{Total: 58 << 30, MinCarSize: 18 << 30, MaxCarSize: 50 << 30}}
	restarts := 0
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sizing: z, Restart: func() { restarts++ }}
	s.Archive = lastRunArchive{st: archive.Status{Last: &archive.Result{Pending: 3}}}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	post := func(body string) (int, string) {
		resp, err := http.Post(ts.URL+"/api/storage/resize", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(`{"size":34359738368,"confirm":"erase"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "ERASE") {
		t.Errorf("no confirm: %d %s", code, body)
	}
	if code, body := post(`{"size":34359738368,"confirm":"ERASE"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "backed up") {
		t.Errorf("not archived: %d %s", code, body)
	}
	s.Archive = lastRunArchive{st: archive.Status{State: archive.Home, Last: &archive.Result{Uploaded: 5}}}
	if code, body := post(`{"size":107374182400,"confirm":"ERASE"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("too big: %d %s", code, body)
	}
	code, body := post(`{"size":34359738368,"confirm":"ERASE"}`)
	if code != http.StatusOK || z.resized != 32<<30 || restarts != 1 {
		t.Fatalf("resize: %d %s, resized %d, restarts %d", code, body, z.resized, restarts)
	}
	var got storageResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Recreate != 32<<30 || !got.Archived || !got.Backups {
		t.Errorf("response %+v, %v", got, err)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/storage/resize", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || z.st.Recreate != 0 {
		t.Errorf("cancel: %d, pending %d", resp.StatusCode, z.st.Recreate)
	}

	// form posts from another site are refused
	resp, err = http.Post(ts.URL+"/api/storage/resize", "text/plain", strings.NewReader(`{"size":34359738368,"confirm":"ERASE"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", resp.StatusCode)
	}
}

type countingArchive struct {
	lastRunArchive
	left int
}

func (a countingArchive) Outstanding() (int, error) { return a.left, nil }

// After a restart there's no last run; what counts is what's left.
func TestArchivedAllCountsWhatsLeft(t *testing.T) {
	s := &Server{Archive: countingArchive{lastRunArchive: lastRunArchive{st: archive.Status{State: archive.Home}}}}
	if archived, backups := s.archivedAll(); !archived || !backups {
		t.Errorf("nothing left, no last run: archived %v, backups %v", archived, backups)
	}
	s.Archive = countingArchive{lastRunArchive: lastRunArchive{st: archive.Status{State: archive.Home}}, left: 3}
	if archived, _ := s.archivedAll(); archived {
		t.Error("3 files left counted as archived")
	}
	s.Archive = countingArchive{lastRunArchive: lastRunArchive{st: archive.Status{State: archive.Archiving}}}
	if archived, _ := s.archivedAll(); archived {
		t.Error("archived while a run is uploading")
	}
}
