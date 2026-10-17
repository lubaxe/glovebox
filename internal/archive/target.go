package archive

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/config"
	"github.com/lubaxe/glovebox/internal/system"
)

// Target is where clips are archived.
type Target interface {
	// Reachable checks quickly whether the target answers, which is how
	// glovebox knows the car is home.
	Reachable(ctx context.Context) error
	Connect(ctx context.Context) (Session, error)
	// Mirror makes dir an exact copy of the folder source on the target
	// (what source means depends on the type; see config.ExtraDriveConfig).
	// It's how the extra drives are filled.
	Mirror(ctx context.Context, source, dir string) error
	String() string
}

// Session is a connection to the target.
type Session interface {
	// Size returns the size of remote, or false if it doesn't exist.
	Size(ctx context.Context, remote string) (int64, bool, error)
	// Upload writes size bytes from r to remote. The file only appears under
	// its final name once it's complete and has the expected size. Targets
	// that run a program to copy (rsync, rclone) need r to be an *os.File
	// whose name ends in remote.
	Upload(ctx context.Context, remote string, r io.ReaderAt, size int64) error
	// Remove deletes the file remote.
	Remove(ctx context.Context, remote string) error
	Close() error
}

// Options are what NewTarget needs besides the config.
type Options struct {
	Runner system.Runner
	// Start starts background processes (an SSH master, rclone's server);
	// nil means system.Start.
	Start system.Starter
	// RunDir holds mount points and sockets, e.g. /run/glovebox.
	RunDir string
}

// NewTarget returns the target the config describes. For SMB, a username
// and password in the config win over the credentials file, so the setup
// page can test a login before saving it.
func NewTarget(c config.ArchiveConfig, o Options) (Target, error) {
	if o.Start == nil {
		o.Start = system.Start
	}
	var t Target
	switch c.Type {
	case config.TypeSMB:
		s := c.SMB
		user, password, domain := s.Username, s.Password, ""
		if user == "" {
			var err error
			if user, password, domain, err = ReadCredentials(s.Credentials); err != nil {
				return nil, fmt.Errorf("reading the SMB login: %w", err)
			}
		}
		dialect, err := ParseDialect(s.Version)
		if err != nil {
			return nil, err
		}
		t = &SMB{
			Server: s.Server, Share: s.Share, Folder: s.Folder,
			User: user, Password: password, Domain: domain,
			Dialect: dialect, Parallel: s.Parallel,
		}
	case config.TypeNFS:
		n := c.NFS
		t = &NFS{Server: n.Server, Export: n.Export, Folder: n.Folder, Options: n.Options,
			MountDir: filepath.Join(o.RunDir, "nfs"), Runner: o.Runner}
	case config.TypeRsync:
		r := c.Rsync
		t = &Rsync{Server: r.Server, Port: r.Port, User: r.User, Path: r.Path, Key: r.Key, KnownHosts: r.KnownHosts,
			ControlDir: o.RunDir, Runner: o.Runner, Start: o.Start}
	case config.TypeRclone:
		r := c.Rclone
		t = &Rclone{Remote: r.Remote, Config: r.Config, Flags: strings.Fields(r.Flags), Runner: o.Runner, Start: o.Start}
	default:
		return nil, fmt.Errorf("unknown archive type %q", c.Type)
	}
	if c.CheckAddress != "" {
		t = &checked{Target: t, addr: c.CheckAddress}
	}
	return t, nil
}

// checked uses its own address to tell whether the car is home.
type checked struct {
	Target
	addr string
}

func (c *checked) Reachable(ctx context.Context) error { return dial(ctx, c.addr) }

// dial checks that addr accepts TCP connections.
func dial(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// CheckStep is one step of a connection test.
type CheckStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// testFile is written and removed again by Check.
const testFile = ".glovebox-test"

// Check tests a target the way archiving uses it: it must answer, accept
// the login, and take, report and delete a small file. It stops at the
// first step that fails.
func Check(ctx context.Context, t Target) []CheckStep {
	var steps []CheckStep
	step := func(name string, err error, ok string) bool {
		s := CheckStep{Name: name, OK: err == nil, Detail: ok}
		if err != nil {
			s.Detail = err.Error()
		}
		steps = append(steps, s)
		return err == nil
	}
	if !step("Reach "+t.String(), t.Reachable(ctx), "") {
		return steps
	}
	sess, err := t.Connect(ctx)
	if !step("Log in", err, "") {
		return steps
	}
	defer sess.Close()

	content := []byte("Glovebox checks that it can write here. Safe to delete.\n")
	err = func() error {
		dir, err := os.MkdirTemp("", "glovebox-check-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		local := filepath.Join(dir, testFile)
		if err := os.WriteFile(local, content, 0o644); err != nil {
			return err
		}
		f, err := os.Open(local)
		if err != nil {
			return err
		}
		defer f.Close()
		return sess.Upload(ctx, testFile, f, int64(len(content)))
	}()
	if !step("Write a test file", err, "") {
		return steps
	}
	size, ok, err := sess.Size(ctx, testFile)
	if err == nil && (!ok || size != int64(len(content))) {
		err = fmt.Errorf("the test file is %d bytes on the target, expected %d", size, len(content))
	}
	if !step("Read it back", err, "") {
		return steps
	}
	step("Delete it", sess.Remove(ctx, testFile), "Everything works.")
	return steps
}

// dirSession is a session on a mounted filesystem.
type dirSession struct {
	root  string
	close func() error
}

func (s *dirSession) full(remote string) string {
	return filepath.Join(s.root, filepath.FromSlash(remote))
}

func (s *dirSession) Size(_ context.Context, remote string) (int64, bool, error) {
	fi, err := os.Stat(s.full(remote))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return fi.Size(), true, nil
}

func (s *dirSession) Upload(ctx context.Context, remote string, r io.ReaderAt, size int64) (err error) {
	full := s.full(remote)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%s.partial", full, nonce())
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	err = parallelCopy(f, r, size, 1, 1<<20)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", remote, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if fi, err := os.Stat(tmp); err != nil {
		return err
	} else if fi.Size() != size {
		return fmt.Errorf("%s is %d bytes on the server, expected %d", remote, fi.Size(), size)
	}
	return os.Rename(tmp, full)
}

func (s *dirSession) Remove(_ context.Context, remote string) error {
	return os.Remove(s.full(remote))
}

func (s *dirSession) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

// nonce makes temporary names unique per attempt.
func nonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

// localPath returns the local file behind r, for targets that copy with a
// program, split into the directory that remote is relative to.
func localPath(r io.ReaderAt, remote string) (base string, err error) {
	f, ok := r.(interface{ Name() string })
	if !ok {
		return "", errors.New("can only upload local files")
	}
	name := filepath.ToSlash(f.Name())
	if !strings.HasSuffix(name, "/"+remote) {
		return "", fmt.Errorf("%s isn't the local copy of %s", name, remote)
	}
	return strings.TrimSuffix(name, remote), nil
}
