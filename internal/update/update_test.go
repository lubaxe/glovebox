package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.4.0", "v0.3.0", true},
		{"v0.3.1", "v0.3.0", true},
		{"v1.0.0", "v0.10.2", true},
		{"v0.10.0", "v0.9.9", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.2.9", "v0.3.0", false},
		{"v0.3.0", "build-20261017-1757-40a9e26", true},
		{"v0.3.0", "dev", true},
		{"build-20261017-1757-40a9e26", "v0.3.0", false},
	} {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestWhatsNew(t *testing.T) {
	body := "## What's new\r\n\r\n- NFS\r\n\r\n## Install\r\n\r\nflash it\r\n"
	if got := whatsNew(body); got != "## What's new\n\n- NFS" {
		t.Errorf("whatsNew = %q", got)
	}
	if got := whatsNew("## Install\n\nflash it"); got != "" {
		t.Errorf("whatsNew of install notes only = %q", got)
	}
}

// fakeGitHub serves a release with the given files.
type fakeGitHub struct {
	tag   string
	files map[string]string
	sums  map[string]string // overrides
	calls int
	mu    sync.Mutex
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/lubaxe/glovebox/releases/latest":
			f.calls++
			type asset struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			}
			assets := []asset{{"SHA256SUMS", ts.URL + "/dl/SHA256SUMS"}}
			for name := range f.files {
				assets = append(assets, asset{name, ts.URL + "/dl/" + name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag, "html_url": "https://github.com/lubaxe/glovebox/releases/tag/" + f.tag,
				"body": "## What's new\n\n- Updates\n\n## Install\n\nflash", "published_at": "2026-10-20T10:00:00Z",
				"assets": assets,
			})
		case r.URL.Path == "/dl/SHA256SUMS":
			for name, data := range f.files {
				sum := sha256.Sum256([]byte(data))
				s := hex.EncodeToString(sum[:])
				if o, ok := f.sums[name]; ok {
					s = o
				}
				fmt.Fprintf(w, "%s  %s\n", s, name)
			}
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			data, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newTest(t *testing.T, gh *fakeGitHub) (*Updater, string, *system.FakeRunner, chan struct{}) {
	dir := t.TempDir()
	ts := gh.server(t)
	restarted := make(chan struct{}, 1)
	runner := &system.FakeRunner{Handler: func(cmdline string) ([]byte, error) {
		if strings.HasSuffix(cmdline, " -version") {
			// the staged binary prints its own version: its contents here
			data, err := os.ReadFile(strings.TrimSuffix(cmdline, " -version"))
			return data, err
		}
		return nil, nil
	}}
	u := &Updater{
		Repo: "lubaxe/glovebox", Current: "v0.3.0", Runner: runner,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Client: ts.Client(), API: ts.URL, MaxAge: time.Hour,
		Restart: func() { restarted <- struct{}{} },
		Files: []File{
			{Asset: "glovebox-linux-arm64", Path: filepath.Join(dir, "glovebox"), Mode: 0o755},
			{Asset: "tesla-control-linux-arm64", Path: filepath.Join(dir, "tesla-control"), Mode: 0o755, Optional: true},
			{Asset: "glovebox.service", Path: filepath.Join(dir, "glovebox.service"), Mode: 0o644, Optional: true},
		},
	}
	for name, data := range map[string]string{"glovebox": "v0.3.0", "tesla-control": "old tc", "glovebox.service": "unit"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return u, dir, runner, restarted
}

func waitIdle(t *testing.T, u *Updater) Status {
	t.Helper()
	for range 200 {
		s := u.Status(context.Background(), false)
		if s.State != "installing" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("still installing")
	return Status{}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCheckAndInstall(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.4.0", files: map[string]string{
		"glovebox-linux-arm64": "v0.4.0", "tesla-control-linux-arm64": "new tc", "glovebox.service": "new unit",
		"glovebox-linux-armv6": "other arch",
	}}
	u, dir, runner, restarted := newTest(t, gh)

	s := u.Status(context.Background(), true)
	if !s.Supported || !s.Release || !s.Available || s.Latest == nil || s.Latest.Version != "v0.4.0" {
		t.Fatalf("status = %+v", s)
	}
	if s.Latest.Notes != "## What's new\n\n- Updates" {
		t.Errorf("notes = %q", s.Latest.Notes)
	}
	// a recent check is reused
	u.Status(context.Background(), true)
	if gh.calls != 1 {
		t.Errorf("GitHub asked %d times, want 1", gh.calls)
	}

	if u.Busy() {
		t.Error("busy before installing")
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	if !u.Busy() {
		t.Error("not busy while installing")
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("no restart; status %+v", u.Status(context.Background(), false))
	}
	if s := u.Status(context.Background(), false); s.State != "restarting" || s.Error != "" || !u.Busy() {
		t.Errorf("status after install = %+v", s)
	}
	for name, want := range map[string]string{"glovebox": "v0.4.0", "tesla-control": "new tc", "glovebox.service": "new unit", "glovebox.old": "v0.3.0", "tesla-control.old": "old tc"} {
		if got := read(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "glovebox")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Errorf("glovebox mode = %v", fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".new-") {
			t.Errorf("left behind %s", e.Name())
		}
	}
	cmds := strings.Join(runner.Commands(), "\n")
	if !strings.Contains(cmds, "systemctl daemon-reload") {
		t.Errorf("no daemon-reload for the new unit: %s", cmds)
	}
	if !strings.Contains(cmds, " -prepare-update") {
		t.Errorf("the new version didn't prepare: %s", cmds)
	}
	// a second install while restarting is refused
	if err := u.Install(); err == nil {
		t.Error("second install allowed")
	}
}

func TestUpToDate(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.3.0", files: map[string]string{"glovebox-linux-arm64": "v0.3.0"}}
	u, _, _, _ := newTest(t, gh)
	if s := u.Check(context.Background()); s.Available || s.Error != "" {
		t.Errorf("status = %+v", s)
	}
	if err := u.Install(); err == nil {
		t.Error("install without an update allowed")
	}
}

func TestBadChecksumChangesNothing(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.4.0",
		files: map[string]string{"glovebox-linux-arm64": "v0.4.0", "tesla-control-linux-arm64": "new tc", "glovebox.service": "unit"},
		sums:  map[string]string{"tesla-control-linux-arm64": strings.Repeat("0", 64)},
	}
	u, dir, _, restarted := newTest(t, gh)
	u.Check(context.Background())
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, u)
	if !strings.Contains(s.Error, "SHA256SUMS") || u.Busy() {
		t.Errorf("error = %q", s.Error)
	}
	if got := read(t, filepath.Join(dir, "glovebox")); got != "v0.3.0" {
		t.Errorf("glovebox replaced: %q", got)
	}
	if len(restarted) != 0 {
		t.Error("restarted after a failed install")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("files left: %v", entries)
	}
}

func TestFailedPrepareChangesNothing(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.4.0", files: map[string]string{"glovebox-linux-arm64": "v0.4.0", "tesla-control-linux-arm64": "new tc", "glovebox.service": "new unit"}}
	u, dir, runner, restarted := newTest(t, gh)
	version := runner.Handler
	runner.Handler = func(cmdline string) ([]byte, error) {
		if strings.HasSuffix(cmdline, " -prepare-update") {
			return nil, errors.New("updating the package lists: offline")
		}
		return version(cmdline)
	}
	u.Check(context.Background())
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, u)
	if !strings.Contains(s.Error, "preparing v0.4.0") || !strings.Contains(s.Error, "offline") {
		t.Errorf("error = %q", s.Error)
	}
	for name, want := range map[string]string{"glovebox": "v0.3.0", "glovebox.service": "unit"} {
		if got := read(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s replaced: %q", name, got)
		}
	}
	if len(restarted) != 0 {
		t.Error("restarted after a failed prepare")
	}
}

func TestWrongVersionRefused(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.4.0", files: map[string]string{"glovebox-linux-arm64": "v0.3.9", "glovebox.service": "unit"}}
	u, dir, _, _ := newTest(t, gh)
	u.Files = u.Files[:1]
	u.Check(context.Background())
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	if s := waitIdle(t, u); !strings.Contains(s.Error, `"v0.3.9"`) {
		t.Errorf("error = %q", s.Error)
	}
	if got := read(t, filepath.Join(dir, "glovebox")); got != "v0.3.0" {
		t.Errorf("glovebox replaced: %q", got)
	}
}

func TestUnsupported(t *testing.T) {
	gh := &fakeGitHub{tag: "v0.4.0"}
	u, _, _, _ := newTest(t, gh)
	u.Unsupported = "no release for windows/amd64"
	if s := u.Check(context.Background()); s.Supported || s.Why == "" || gh.calls != 0 {
		t.Errorf("status = %+v, calls %d", s, gh.calls)
	}
	if err := u.Install(); err == nil {
		t.Error("install allowed")
	}
}

func TestGitHubError(t *testing.T) {
	u, _, _, _ := newTest(t, &fakeGitHub{tag: "v0.4.0"})
	u.Repo = "lubaxe/missing"
	if s := u.Check(context.Background()); s.Error == "" || s.Available {
		t.Errorf("status = %+v", s)
	}
}
