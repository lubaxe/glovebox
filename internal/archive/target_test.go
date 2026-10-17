package archive

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

func TestNewTarget(t *testing.T) {
	creds := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(creds, []byte("username=carcam\npassword=pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.Default().Archive
	c.SMB.Server, c.SMB.Credentials = "192.168.1.10", creds
	c.NFS.Server, c.NFS.Export = "192.168.1.10", "/mnt/user/TeslaCam"
	c.Rsync.Server, c.Rsync.User, c.Rsync.Path = "192.168.1.10", "carcam", "/volume1/TeslaCam"
	c.Rclone.Remote = "gdrive:TeslaCam"
	for typ, want := range map[string]string{
		config.TypeSMB:    "//192.168.1.10/TeslaCam",
		config.TypeNFS:    "192.168.1.10:/mnt/user/TeslaCam",
		config.TypeRsync:  "carcam@192.168.1.10:/volume1/TeslaCam",
		config.TypeRclone: "gdrive:TeslaCam",
	} {
		c.Type = typ
		tg, err := NewTarget(c, Options{Runner: &system.FakeRunner{}, RunDir: t.TempDir()})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if tg.String() != want {
			t.Errorf("%s: String() = %q, want %q", typ, tg.String(), want)
		}
	}

	// a login given directly wins over the file (the setup page tests it)
	c.Type = config.TypeSMB
	c.SMB.Username, c.SMB.Password = "other", "pw2"
	tg, _ := NewTarget(c, Options{})
	if s := tg.(*SMB); s.User != "other" || s.Password != "pw2" {
		t.Errorf("login = %s/%s", s.User, s.Password)
	}
	c.SMB.Username, c.SMB.Credentials = "", filepath.Join(t.TempDir(), "missing")
	if _, err := NewTarget(c, Options{}); err == nil {
		t.Error("missing credentials file: want an error")
	}
}

func TestCheckAddressDecidesReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := config.Default().Archive
	c.Type, c.Rclone.Remote, c.CheckAddress = config.TypeRclone, "gdrive:TeslaCam", l.Addr().String()
	tg, err := NewTarget(c, Options{Runner: &system.FakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.Reachable(context.Background()); err != nil {
		t.Errorf("Reachable = %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	tg, _ = NewTarget(c, Options{Runner: &system.FakeRunner{}})
	if err := tg.Reachable(context.Background()); err == nil {
		t.Errorf("Reachable with %s closed = nil, want an error", addr)
	}
}

func TestDirSession(t *testing.T) {
	root := t.TempDir()
	s := &dirSession{root: root}
	ctx := context.Background()
	if _, ok, err := s.Size(ctx, "SentryClips/a/b.mp4"); ok || err != nil {
		t.Fatalf("Size of missing file = %v, %v", ok, err)
	}
	data := strings.Repeat("x", 3<<20+5)
	if err := s.Upload(ctx, "SentryClips/a/b.mp4", strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.Size(ctx, "SentryClips/a/b.mp4"); !ok || err != nil || n != int64(len(data)) {
		t.Errorf("Size = %d, %v, %v", n, ok, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "SentryClips", "a"))
	if len(entries) != 1 {
		t.Errorf("leftover temporary files: %v", entries)
	}
	if err := s.Upload(ctx, "short", strings.NewReader("abc"), 10); err == nil {
		t.Error("short source: want an error")
	}
	if err := s.Remove(ctx, "SentryClips/a/b.mp4"); err != nil {
		t.Error(err)
	}
}

// dirTarget is a target in a local folder, for Check.
type dirTarget struct {
	root    string
	connErr error
}

func (d *dirTarget) String() string                               { return d.root }
func (d *dirTarget) Reachable(context.Context) error              { return nil }
func (d *dirTarget) Mirror(context.Context, string, string) error { return nil }
func (d *dirTarget) Connect(context.Context) (Session, error) {
	if d.connErr != nil {
		return nil, d.connErr
	}
	return &dirSession{root: d.root}, nil
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	steps := Check(context.Background(), &dirTarget{root: root})
	if len(steps) != 5 {
		t.Fatalf("steps = %+v", steps)
	}
	for _, s := range steps {
		if !s.OK {
			t.Errorf("step failed: %+v", s)
		}
	}
	if _, err := os.Stat(filepath.Join(root, testFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("test file left behind")
	}

	steps = Check(context.Background(), &dirTarget{root: root, connErr: errors.New("bad password")})
	if len(steps) != 2 || steps[1].OK || steps[1].Detail != "bad password" {
		t.Errorf("steps = %+v", steps)
	}
}

func TestMirrorFS(t *testing.T) {
	old := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	src := fstest.MapFS{
		"Album/01 Song.mp3":   {Data: []byte("song one"), ModTime: old},
		"Album/02 Song.mp3":   {Data: []byte("song two, new"), ModTime: old},
		"Album/._01 Song.mp3": {Data: []byte("mac litter"), ModTime: old},
		".DS_Store":           {Data: []byte("mac litter"), ModTime: old},
		"Empty":               {Mode: os.ModeDir | 0o755},
		"single.flac":         {Data: []byte("flac"), ModTime: old},
	}
	dst := t.TempDir()
	write := func(rel, content string, mtime time.Time) {
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("Album/01 Song.mp3", "SONG ONE", old.Add(time.Second)) // same size and time: left alone
	write("Album/02 Song.mp3", "song two", old)                  // changed size: copied
	write("Gone/old.mp3", "old", old)                            // not in src: deleted
	write("System Volume Information/x", "windows", old)         // excluded: left alone

	if err := mirrorFS(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	for rel, want := range map[string]string{
		"Album/01 Song.mp3":           "SONG ONE",
		"Album/02 Song.mp3":           "song two, new",
		"single.flac":                 "flac",
		"Album/._01 Song.mp3":         "<missing>",
		".DS_Store":                   "<missing>",
		"Gone/old.mp3":                "<missing>",
		"System Volume Information/x": "windows",
	} {
		if got := read(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "Gone")); !errors.Is(err, os.ErrNotExist) {
		t.Error("folder Gone should be deleted")
	}
	if fi, err := os.Stat(filepath.Join(dst, "Empty")); err != nil || !fi.IsDir() {
		t.Error("empty folder should be created")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "single.flac")); !within(fi.ModTime(), old) {
		t.Errorf("modification time %v, want %v", fi.ModTime(), old)
	}
}

func TestNFSMountsOnlyWhileConnected(t *testing.T) {
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "mountpoint") {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 1}
		}
		return nil, nil
	}}
	dir := t.TempDir()
	n := &NFS{Server: "192.168.1.10", Export: "/mnt/user/TeslaCam", Folder: "car", Options: "vers=3,soft", MountDir: dir, Runner: r}
	s, err := n.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(dir, "archive")
	want := []string{
		"mountpoint -q " + mnt,
		"mount -t nfs -o rw,vers=3,soft 192.168.1.10:/mnt/user/TeslaCam " + mnt,
		"umount -f -l " + mnt,
	}
	if got := r.Commands(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := n.Mirror(context.Background(), "/mnt/user/Music", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cmds := strings.Join(r.Commands(), "\n"); !strings.Contains(cmds, "mount -t nfs -o ro,vers=3,soft 192.168.1.10:/mnt/user/Music") {
		t.Errorf("music isn't mounted read-only:\n%s", cmds)
	}
}

// fakeProc is a background process that runs until stopped.
type fakeProc struct {
	once   sync.Once
	exited chan struct{}
	out    string
}

func newFakeProc() *fakeProc                { return &fakeProc{exited: make(chan struct{})} }
func (p *fakeProc) Exited() <-chan struct{} { return p.exited }
func (p *fakeProc) Output() string          { return p.out }
func (p *fakeProc) Stop() error             { p.once.Do(func() { close(p.exited) }); return nil }

func newRsync(t *testing.T, r *system.FakeRunner, started *[]string) *Rsync {
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Rsync{
		Server: "nas", Port: 22, User: "carcam", Path: "/volume1/TeslaCam",
		Key: key, KnownHosts: filepath.Join(t.TempDir(), "ssh", "known_hosts"),
		ControlDir: t.TempDir(), Runner: r,
		Start: func(name string, args ...string) (system.Process, error) {
			*started = append(*started, name+" "+strings.Join(args, " "))
			return newFakeProc(), nil
		},
	}
}

func TestRsyncSession(t *testing.T) {
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		switch {
		case strings.Contains(cmd, "--list-only") && strings.HasSuffix(cmd, "/missing.mp4"):
			return []byte("rsync: [sender] link_stat \"/volume1/TeslaCam/missing.mp4\" failed: No such file or directory (2)\n"),
				&system.CmdError{Cmd: cmd, ExitCode: 23}
		case strings.Contains(cmd, "--list-only"):
			return []byte("-rw-r--r--       1,234,567 2026/10/15 09:00:00 x.mp4\n"), nil
		}
		return nil, nil
	}}
	var started []string
	tg := newRsync(t, r, &started)
	s, err := tg.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !strings.Contains(started[0], "-M -N -S ") || !strings.HasSuffix(started[0], "carcam@nas") {
		t.Fatalf("master: %v", started)
	}
	ctx := context.Background()
	if n, ok, err := s.Size(ctx, "SentryClips/x.mp4"); n != 1234567 || !ok || err != nil {
		t.Errorf("Size = %d, %v, %v", n, ok, err)
	}
	if _, ok, err := s.Size(ctx, "missing.mp4"); ok || err != nil {
		t.Errorf("Size of missing = %v, %v", ok, err)
	}

	snap := t.TempDir()
	local := filepath.Join(snap, "TeslaCam", "SentryClips", "x.mp4")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(local)
	defer f.Close()
	if err := s.Upload(ctx, "SentryClips/x.mp4", f, 4); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(ctx, "SentryClips/other.mp4", f, 4); err == nil {
		t.Error("upload of a file under the wrong name: want an error")
	}
	if err := s.Remove(ctx, ".glovebox-test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	cmds := r.Commands()
	find := func(part string) string {
		for _, c := range cmds {
			if strings.Contains(c, part) {
				return c
			}
		}
		t.Errorf("no command with %q in:\n%s", part, strings.Join(cmds, "\n"))
		return ""
	}
	up := find("-tR")
	if !strings.Contains(up, filepath.ToSlash(snap)+"/TeslaCam/./SentryClips/x.mp4 carcam@nas:/volume1/TeslaCam/") {
		t.Errorf("upload: %s", up)
	}
	if !strings.Contains(up, "'-o' 'ControlMaster=no'") {
		t.Errorf("upload doesn't reuse the connection: %s", up)
	}
	if rm := find("--delete"); !strings.Contains(rm, "--include=/.glovebox-test --exclude=*") || !strings.HasSuffix(rm, "carcam@nas:/volume1/TeslaCam/") {
		t.Errorf("remove: %s", rm)
	}
	find("-O exit")
}

func TestRsyncLoginFailure(t *testing.T) {
	r := &system.FakeRunner{Handler: func(cmd string) ([]byte, error) {
		return nil, &system.CmdError{Cmd: cmd, ExitCode: 255}
	}}
	var started []string
	tg := newRsync(t, r, &started)
	tg.Start = func(string, ...string) (system.Process, error) {
		p := newFakeProc()
		p.out = "carcam@nas: Permission denied (publickey)."
		p.Stop()
		return p, nil
	}
	_, err := tg.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Errorf("err = %v", err)
	}
}

func TestRsyncMirror(t *testing.T) {
	r := &system.FakeRunner{}
	var started []string
	tg := newRsync(t, r, &started)
	if err := tg.Mirror(context.Background(), "Music/", "/tmp/mnt"); err != nil {
		t.Fatal(err)
	}
	cmd := r.Commands()[0]
	for _, want := range []string{"rsync -s -rt --delete ", "--exclude=.DS_Store", "carcam@nas:Music/ /tmp/mnt/"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("%q not in %s", want, cmd)
		}
	}
}

// fakeRC is enough of rclone's remote control API for the session.
type fakeRC struct {
	mu    sync.Mutex
	files map[string]int64
	calls []string
}

func (f *fakeRC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.calls = append(f.calls, method)
	fail := func(msg string) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	switch method {
	case "operations/list":
		_, _ = w.Write([]byte(`{"list":[]}`))
	case "operations/stat":
		if n, ok := f.files[in["remote"].(string)]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"Size": n}})
		} else {
			_, _ = w.Write([]byte(`{"item":null}`))
		}
	case "operations/copyfile":
		if in["dstFs"] != "gdrive:TeslaCam/" {
			fail("wrong dstFs")
			return
		}
		fi, err := os.Stat(filepath.Join(in["srcFs"].(string), in["srcRemote"].(string)))
		if err != nil {
			fail(err.Error())
			return
		}
		f.files[in["dstRemote"].(string)] = fi.Size()
		_, _ = w.Write([]byte(`{}`))
	case "operations/deletefile":
		delete(f.files, in["remote"].(string))
		_, _ = w.Write([]byte(`{}`))
	default:
		fail("unknown method " + method)
	}
}

func TestRcloneSession(t *testing.T) {
	rc := &fakeRC{files: map[string]int64{}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	tg := &Rclone{Remote: "gdrive:TeslaCam", Config: "/etc/glovebox/rclone.conf",
		serve: func(context.Context) (*rcClient, error) {
			return &rcClient{url: srv.URL + "/", user: "u", pass: "p"}, nil
		}}
	steps := Check(context.Background(), &rcloneOnDisk{tg})
	for _, s := range steps {
		if !s.OK {
			t.Errorf("step failed: %+v", s)
		}
	}
	if len(steps) != 5 {
		t.Errorf("steps = %+v", steps)
	}
	if len(rc.files) != 0 {
		t.Errorf("test file left: %v", rc.files)
	}
	if !strings.Contains(strings.Join(rc.calls, " "), "operations/copyfile operations/stat operations/deletefile") {
		t.Errorf("calls = %v", rc.calls)
	}
}

// rcloneOnDisk skips the check address, which Check would otherwise dial.
type rcloneOnDisk struct{ *Rclone }

func (rcloneOnDisk) Reachable(context.Context) error { return nil }

func TestRcloneMirror(t *testing.T) {
	r := &system.FakeRunner{}
	tg := &Rclone{Remote: "gdrive:TeslaCam", Config: "/c/rclone.conf", Flags: []string{"--bwlimit", "2M"}, Runner: r}
	for source, want := range map[string]string{"Music": "gdrive:Music", "nas:Lights": "nas:Lights"} {
		if err := tg.Mirror(context.Background(), source, "/tmp/mnt"); err != nil {
			t.Fatal(err)
		}
		cmd := r.Commands()[len(r.Commands())-1]
		if !strings.HasPrefix(cmd, "rclone --config /c/rclone.conf sync "+want+" /tmp/mnt --modify-window 2s") || !strings.HasSuffix(cmd, "--bwlimit 2M") {
			t.Errorf("command: %s", cmd)
		}
	}
}
