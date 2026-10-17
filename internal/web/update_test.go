package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/update"
)

type fakeUpdater struct {
	checks, installs int
	installErr       error
}

func (f *fakeUpdater) Status(ctx context.Context, check bool) update.Status {
	if check {
		f.checks++
	}
	return update.Status{Current: "v0.3.0", State: "idle", Supported: true}
}

func (f *fakeUpdater) Check(ctx context.Context) update.Status {
	f.checks++
	return update.Status{Current: "v0.3.0", State: "idle", Supported: true, Available: true, Latest: &update.Release{Version: "v0.4.0"}}
}

func (f *fakeUpdater) Install() error {
	f.installs++
	return f.installErr
}

func TestUpdateRoutes(t *testing.T) {
	up := &fakeUpdater{}
	s := &Server{Version: "v0.3.0", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Update: up}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	post := func(path, ctype string) *http.Response {
		t.Helper()
		resp, err := http.Post(ts.URL+path, ctype, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp, err := http.Get(ts.URL + "/api/update?check=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || up.checks != 1 {
		t.Errorf("GET: status %d, checks %d", resp.StatusCode, up.checks)
	}
	if r := post("/api/update/install", "text/plain"); r.StatusCode != http.StatusUnsupportedMediaType || up.installs != 0 {
		t.Errorf("install without JSON: %d, installs %d", r.StatusCode, up.installs)
	}
	if r := post("/api/update/check", "application/json"); r.StatusCode != http.StatusOK || up.checks != 2 {
		t.Errorf("check: %d, checks %d", r.StatusCode, up.checks)
	}
	if r := post("/api/update/install", "application/json"); r.StatusCode != http.StatusOK || up.installs != 1 {
		t.Errorf("install: %d, installs %d", r.StatusCode, up.installs)
	}
	up.installErr = errors.New("there's no update to install")
	if r := post("/api/update/install", "application/json"); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("failed install: %d", r.StatusCode)
	}
}

func TestUpdateUnavailable(t *testing.T) {
	ts := newServer(fakeStatus{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/update")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
