package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/setup"
)

type fakeSetup struct {
	saved   []setup.Settings
	saveErr error
}

func (f *fakeSetup) Get() (setup.Settings, error) {
	return setup.Settings{Archive: setup.Archive{Type: "smb"}}, nil
}
func (f *fakeSetup) Test(_ context.Context, in setup.Settings) []archive.CheckStep {
	return []archive.CheckStep{{Name: "Reach " + in.Archive.SMB.Server, OK: true}}
}
func (f *fakeSetup) Save(in setup.Settings) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, in)
	return nil
}
func (f *fakeSetup) SSHKey(context.Context) (string, error) { return "ssh-ed25519 AAAA glovebox", nil }
func (f *fakeSetup) SetRcloneConfig(text string) ([]string, error) {
	if text == "" {
		return nil, errors.New("no remotes")
	}
	return []string{"gdrive"}, nil
}

func setupServer(f *fakeSetup, restarted *int) *httptest.Server {
	s := &Server{Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Setup: f,
		Restart: func() { *restarted++ }}
	return httptest.NewServer(s.Handler())
}

func call(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestSetupRoutes(t *testing.T) {
	f := &fakeSetup{}
	restarts := 0
	ts := setupServer(f, &restarts)
	defer ts.Close()

	if code, out := call(t, "GET", ts.URL+"/api/setup", ""); code != 200 || out["archive"].(map[string]any)["type"] != "smb" {
		t.Errorf("get: %d %v", code, out)
	}
	code, out := call(t, "POST", ts.URL+"/api/setup/test", `{"archive":{"type":"smb","smb":{"server":"192.168.1.10"}}}`)
	if steps, _ := out["steps"].([]any); code != 200 || len(steps) != 1 || steps[0].(map[string]any)["name"] != "Reach 192.168.1.10" {
		t.Errorf("test: %d %v", code, out)
	}
	if code, out := call(t, "PUT", ts.URL+"/api/setup", `{"archive":{"enabled":true}}`); code != 200 || out["restarting"] != true || restarts != 1 || len(f.saved) != 1 {
		t.Errorf("save: %d %v restarts=%d", code, out, restarts)
	}
	f.saveErr = errors.New("archive.smb.server must be set")
	if code, out := call(t, "PUT", ts.URL+"/api/setup", `{}`); code != 422 || out["error"] != "archive.smb.server must be set" || restarts != 1 {
		t.Errorf("bad save: %d %v", code, out)
	}
	if code, out := call(t, "POST", ts.URL+"/api/setup/ssh-key", `{}`); code != 200 || out["public_key"] != "ssh-ed25519 AAAA glovebox" {
		t.Errorf("ssh key: %d %v", code, out)
	}
	if code, _ := call(t, "PUT", ts.URL+"/api/setup/rclone-config", `{"config":""}`); code != 422 {
		t.Errorf("empty rclone config: %d", code)
	}

	// a form from another site can't change anything
	res, err := http.Post(ts.URL+"/api/setup", "application/x-www-form-urlencoded", strings.NewReader("archive=1"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Error("a form post was accepted")
	}
}
