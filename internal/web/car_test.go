package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/keepawake"
	"github.com/lubaxe/glovebox/internal/system"
)

func sendJSON(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestCarAPI(t *testing.T) {
	r := &system.FakeRunner{}
	car := &keepawake.Car{Command: "tesla-control", Dir: filepath.Join(t.TempDir(), "ble"), Runner: r}
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Car: car}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var got carResponse
	decode := func(resp *http.Response) {
		t.Helper()
		got = carResponse{}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
	}
	decode(do(t, "GET", ts.URL+"/api/car"))
	if got.VIN != "" || got.Paired {
		t.Errorf("fresh: %+v", got)
	}

	if resp := sendJSON(t, "PUT", ts.URL+"/api/car/vin", `{"vin":"nope"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad VIN: status %d", resp.StatusCode)
	}
	decode(sendJSON(t, "PUT", ts.URL+"/api/car/vin", `{"vin":"5yj3e1ea0pf000001"}`))
	if got.VIN != "5YJ3E1EA0PF000001" {
		t.Errorf("after setting the VIN: %+v", got)
	}
	if resp := sendJSON(t, "POST", ts.URL+"/api/car/pair", `{}`); resp.StatusCode != http.StatusOK {
		t.Errorf("pair: status %d", resp.StatusCode)
	}

	// the car hasn't accepted the key: its error is passed on
	r.Handler = func(string) ([]byte, error) {
		return nil, &system.CmdError{Cmd: "tesla-control", ExitCode: 1, Output: "Failed to execute command: key not paired\n"}
	}
	resp := sendJSON(t, "POST", ts.URL+"/api/car/check", `{}`)
	var e map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if resp.StatusCode != http.StatusUnprocessableEntity || e["error"] != "Failed to execute command: key not paired" {
		t.Errorf("check before accepting: %d %v", resp.StatusCode, e)
	}
	r.Handler = nil
	decode(sendJSON(t, "POST", ts.URL+"/api/car/check", `{}`))
	if !got.Paired || got.PairedAt == nil {
		t.Errorf("after the car accepted: %+v", got)
	}

	// a form posted from another site is refused
	form, _ := http.Post(ts.URL+"/api/car/forget", "application/x-www-form-urlencoded", strings.NewReader("x=1"))
	if form.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: status %d", form.StatusCode)
	}
	decode(sendJSON(t, "POST", ts.URL+"/api/car/forget", `{}`))
	if got.Paired || got.VIN != "" {
		t.Errorf("after forgetting: %+v", got)
	}
}

func TestCarAPIWhenOff(t *testing.T) {
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp := do(t, "GET", ts.URL+"/api/car"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d", resp.StatusCode)
	}
}

type fakeHold struct{ d time.Duration }

func (f *fakeHold) Status() keepawake.Status { return keepawake.Status{HeldUntil: time.Now().Add(f.d)} }
func (f *fakeHold) Hold(d time.Duration)     { f.d = d }

func TestKeepAwakeHold(t *testing.T) {
	car := &keepawake.Car{Command: "tesla-control", Dir: filepath.Join(t.TempDir(), "ble"), Runner: &system.FakeRunner{}}
	k := &fakeHold{}
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Car: car, KeepAwake: k}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if resp := sendJSON(t, "POST", ts.URL+"/api/car/keep-awake", `{"minutes":30}`); resp.StatusCode != http.StatusOK || k.d != 30*time.Minute {
		t.Errorf("hold: status %d, %v", resp.StatusCode, k.d)
	}
	if sendJSON(t, "POST", ts.URL+"/api/car/keep-awake", `{"minutes":600}`); k.d != maxHold {
		t.Errorf("hold is capped at %v, got %v", maxHold, k.d)
	}
	if sendJSON(t, "POST", ts.URL+"/api/car/keep-awake", `{"minutes":0}`); k.d != 0 {
		t.Errorf("stop: %v", k.d)
	}
	if resp := sendJSON(t, "POST", ts.URL+"/api/car/keep-awake", `{"minutes":-1}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative: status %d", resp.StatusCode)
	}
}
