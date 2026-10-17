package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lubaxe/glovebox/internal/archive"
)

type fakeArchive struct{ triggered int }

func (a *fakeArchive) Status() archive.Status {
	return archive.Status{Target: "//nas/TeslaCam", State: archive.Home, Archived: 12}
}
func (a *fakeArchive) Trigger() { a.triggered++ }

func TestArchiveAPI(t *testing.T) {
	a := &fakeArchive{}
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Archive: a}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var st archive.Status
	if err := json.NewDecoder(do(t, "GET", ts.URL+"/api/archive").Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != archive.Home || st.Archived != 12 {
		t.Errorf("status = %+v", st)
	}
	if resp := do(t, "POST", ts.URL+"/api/archive/run"); resp.StatusCode != http.StatusAccepted || a.triggered != 1 {
		t.Errorf("run: status = %d, triggered = %d", resp.StatusCode, a.triggered)
	}

	s.Archive = nil
	ts2 := httptest.NewServer(s.Handler())
	defer ts2.Close()
	if resp := do(t, "GET", ts2.URL+"/api/archive"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("disabled: status = %d", resp.StatusCode)
	}
}
