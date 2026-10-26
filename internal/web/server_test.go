package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lubaxe/glovebox/internal/sysinfo"
)

type fakeStatus struct {
	info sysinfo.Info
	err  error
}

func (f fakeStatus) Read() (sysinfo.Info, error) { return f.info, f.err }

func newServer(status StatusSource) *httptest.Server {
	s := &Server{Version: "test", Status: status, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return httptest.NewServer(s.Handler())
}

func TestStatus(t *testing.T) {
	ts := newServer(fakeStatus{info: sysinfo.Info{Hostname: "glovebox", UptimeSec: 12}})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "test" || got.System.Hostname != "glovebox" || got.System.UptimeSec != 12 {
		t.Errorf("got %+v", got)
	}
}

func TestStatusError(t *testing.T) {
	ts := newServer(fakeStatus{err: errors.New("no /proc")})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestStatusWarnings(t *testing.T) {
	warning := "Persistent boot protection setup failed; setup will retry on restart"
	server := &Server{Version: "test", Status: fakeStatus{}, Warnings: func() []string { return []string{warning} }}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var result statusResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != warning {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

func TestUnknownPath(t *testing.T) {
	ts := newServer(fakeStatus{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
