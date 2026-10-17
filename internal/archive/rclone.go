package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// Rclone archives to any rclone remote. While archiving, rclone runs as a
// server on localhost ("rclone rcd") that's sent one request per file, so
// rclone starts, and logs in to the remote, only once per session.
type Rclone struct {
	Remote string // e.g. "gdrive:TeslaCam"
	Config string // rclone.conf
	Flags  []string
	Runner system.Runner
	Start  system.Starter
	// serve starts rclone's server; nil means startRCD. Tests replace it.
	serve func(ctx context.Context) (*rcClient, error)
}

func (t *Rclone) String() string { return t.Remote }

// Reachable implements Target. An rclone remote can be anywhere, so the
// config's check_address says whether the car is home (see checked).
func (t *Rclone) Reachable(context.Context) error {
	return errors.New("archive.check_address isn't set")
}

func (t *Rclone) args(args ...string) []string {
	return append(append([]string{"--config", t.Config}, args...), t.Flags...)
}

// Connect implements Target.
func (t *Rclone) Connect(ctx context.Context) (Session, error) {
	serve := t.serve
	if serve == nil {
		serve = t.startRCD
	}
	c, err := serve(ctx)
	if err != nil {
		return nil, err
	}
	// look at the remote now, so a bad config fails here, like a bad login;
	// a folder that isn't there yet is created by the first upload
	var out struct{}
	opt := map[string]any{"dirsOnly": true}
	if err := c.call(ctx, "operations/list", map[string]any{"fs": t.root(), "remote": "", "opt": opt}, &out); err != nil && !strings.Contains(err.Error(), "not found") {
		c.close()
		return nil, fmt.Errorf("opening %s: %w", t.Remote, err)
	}
	return &rcloneSession{t: t, c: c}, nil
}

// root is the remote's fs string, ending in ":" or "/".
func (t *Rclone) root() string {
	if strings.HasSuffix(t.Remote, ":") {
		return t.Remote
	}
	return strings.TrimSuffix(t.Remote, "/") + "/"
}

// Mirror implements Target. source is "remote:path", or a path on the
// archive's remote.
func (t *Rclone) Mirror(ctx context.Context, source, dir string) error {
	if !strings.Contains(source, ":") {
		name, _, _ := strings.Cut(t.Remote, ":")
		source = name + ":" + source
	}
	args := t.args(append([]string{"sync", source, dir, "--modify-window", "2s"}, excludeFlags()...)...)
	if _, err := t.Runner.Run(ctx, "rclone", args...); err != nil {
		return fmt.Errorf("syncing %s: %w", source, err)
	}
	return nil
}

// startRCD starts "rclone rcd" on a free localhost port with a one-time
// login and waits until it answers.
func (t *Rclone) startRCD(ctx context.Context) (*rcClient, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	l.Close()
	c := &rcClient{url: "http://" + addr + "/", user: "glovebox", pass: nonce() + nonce()}
	proc, err := t.Start("rclone", t.args("rcd", "--rc-addr", addr, "--rc-user", c.user, "--rc-pass", c.pass)...)
	if err != nil {
		return nil, err
	}
	c.proc = proc
	deadline := time.Now().Add(30 * time.Second) // slow to start on a Pi Zero
	for {
		var out struct{}
		if err := c.call(ctx, "rc/noop", map[string]any{}, &out); err == nil {
			return c, nil
		}
		select {
		case <-proc.Exited():
			return nil, fmt.Errorf("rclone: %s", proc.Output())
		case <-ctx.Done():
			c.close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			c.close()
			return nil, errors.New("rclone didn't start")
		}
	}
}

// rcClient talks to rclone's remote control API.
type rcClient struct {
	url, user, pass string
	proc            system.Process
	http            http.Client
}

func (c *rcClient) call(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("rclone %s: HTTP %d", method, res.StatusCode)
	}
	return json.Unmarshal(b, out)
}

func (c *rcClient) close() error {
	if c.proc == nil {
		return nil
	}
	return c.proc.Stop()
}

type rcloneSession struct {
	t *Rclone
	c *rcClient
}

func (s *rcloneSession) Size(ctx context.Context, remote string) (int64, bool, error) {
	var out struct {
		Item *struct {
			Size int64 `json:"Size"`
		} `json:"item"`
	}
	err := s.c.call(ctx, "operations/stat", map[string]any{"fs": s.t.root(), "remote": remote}, &out)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return 0, false, nil
		}
		return 0, false, err
	}
	if out.Item == nil {
		return 0, false, nil
	}
	return out.Item.Size, true, nil
}

func (s *rcloneSession) Upload(ctx context.Context, remote string, r io.ReaderAt, size int64) error {
	base, err := localPath(r, remote)
	if err != nil {
		return err
	}
	var out struct{}
	return s.c.call(ctx, "operations/copyfile", map[string]any{
		"srcFs": base, "srcRemote": remote,
		"dstFs": s.t.root(), "dstRemote": remote,
	}, &out)
}

func (s *rcloneSession) Remove(ctx context.Context, remote string) error {
	var out struct{}
	return s.c.call(ctx, "operations/deletefile", map[string]any{"fs": s.t.root(), "remote": path.Clean(remote)}, &out)
}

func (s *rcloneSession) Close() error { return s.c.close() }
