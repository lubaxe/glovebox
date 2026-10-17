package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lubaxe/glovebox/internal/system"
)

// Rsync archives to a folder on a server with rsync over SSH. One SSH
// connection is kept open per session and shared by every rsync, so each
// file doesn't pay for an SSH handshake, which takes the Pi Zero 2 W about
// a second.
type Rsync struct {
	Server string
	Port   int
	User   string
	Path   string // folder on the server
	// Key is Glovebox's private key; KnownHosts remembers the server's key
	// from the first connection on.
	Key, KnownHosts string
	// ControlDir holds the shared connection's socket.
	ControlDir string
	Runner     system.Runner
	Start      system.Starter
}

func (t *Rsync) String() string { return fmt.Sprintf("%s@%s:%s", t.User, t.Server, t.Path) }

func (t *Rsync) dest() string { return t.User + "@" + t.Server }

// sshArgs are the options for every ssh command.
func (t *Rsync) sshArgs() []string {
	return []string{
		"-p", strconv.Itoa(t.Port), "-i", t.Key,
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + t.KnownHosts,
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
	}
}

// rsh is the ssh command for rsync's -e, which splits it on spaces and
// honors quotes.
func rsh(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return "ssh " + strings.Join(q, " ")
}

// Reachable implements Target.
func (t *Rsync) Reachable(ctx context.Context) error {
	return dial(ctx, hostPort(t.Server, t.Port))
}

// Connect implements Target: it opens the shared SSH connection.
func (t *Rsync) Connect(ctx context.Context) (Session, error) {
	if _, err := os.Stat(t.Key); err != nil {
		return nil, fmt.Errorf("no SSH key for Glovebox (%w); create one on the setup page", err)
	}
	if err := os.MkdirAll(t.ControlDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(t.KnownHosts), 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(t.ControlDir, "ssh-"+nonce()+".sock")
	args := append(t.sshArgs(), "-M", "-N", "-S", sock, "-o", "ControlPersist=no", t.dest())
	proc, err := t.Start("ssh", args...)
	if err != nil {
		return nil, err
	}
	s := &rsyncSession{t: t, sock: sock, proc: proc}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := t.Runner.Run(ctx, "ssh", "-S", sock, "-O", "check", t.dest()); err == nil {
			return s, nil
		}
		select {
		case <-proc.Exited():
			return nil, fmt.Errorf("logging in to %s: %s", t.dest(), sshError(proc.Output()))
		case <-ctx.Done():
			s.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			s.Close()
			return nil, fmt.Errorf("logging in to %s: timed out", t.dest())
		}
	}
}

// sshError makes ssh's complaints readable.
func sshError(out string) string {
	switch {
	case out == "":
		return "ssh exited"
	case strings.Contains(out, "Permission denied"):
		return out + " (is Glovebox's public key in the server's authorized_keys?)"
	case strings.Contains(out, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return "the server's SSH key changed; if that's expected, clear the known host on the setup page"
	}
	return out
}

// Mirror implements Target. source is a path on the server, relative to
// the user's home unless it starts with /.
func (t *Rsync) Mirror(ctx context.Context, source, dir string) error {
	args := append([]string{"-s", "-rt", "--delete", "--prune-empty-dirs", "--modify-window=2", "--timeout=60"}, excludeFlags()...)
	args = append(args, "-e", rsh(t.sshArgs()), t.dest()+":"+strings.TrimSuffix(source, "/")+"/", dir+"/")
	if _, err := t.Runner.Run(ctx, "rsync", args...); err != nil {
		return fmt.Errorf("syncing %s: %w", source, err)
	}
	return nil
}

type rsyncSession struct {
	t    *Rsync
	sock string
	proc system.Process
}

func (s *rsyncSession) rsh() string {
	return rsh(append(s.t.sshArgs(), "-S", s.sock, "-o", "ControlMaster=no"))
}

func (s *rsyncSession) remote(remote string) string {
	return s.t.dest() + ":" + path.Join(s.t.Path, remote)
}

func (s *rsyncSession) Size(ctx context.Context, remote string) (int64, bool, error) {
	out, err := s.t.Runner.Run(ctx, "rsync", "-s", "--list-only", "--no-human-readable", "--timeout=60", "-e", s.rsh(), s.remote(remote))
	if err != nil {
		if system.ExitCode(err) == 23 && strings.Contains(string(out)+err.Error(), "No such file") {
			return 0, false, nil
		}
		return 0, false, err
	}
	// -rw-r--r--      12345678 2026/10/15 09:00:00 name
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.HasPrefix(f[0], "-") {
			n, err := strconv.ParseInt(strings.ReplaceAll(f[1], ",", ""), 10, 64)
			if err != nil {
				return 0, false, fmt.Errorf("reading the size of %s from %q", remote, line)
			}
			return n, true, nil
		}
	}
	return 0, false, nil
}

func (s *rsyncSession) Upload(ctx context.Context, remote string, r io.ReaderAt, size int64) error {
	base, err := localPath(r, remote)
	if err != nil {
		return err
	}
	// -R with "/./" recreates remote's folders on the server; rsync writes
	// to a temporary name and renames it once complete.
	src := base + "./" + remote
	_, err = s.t.Runner.Run(ctx, "rsync", "-s", "-tR", "--timeout=60", "-e", s.rsh(), src, s.t.dest()+":"+s.t.Path+"/")
	return err
}

// Remove deletes remote with rsync itself, so it also works on servers that
// only allow rsync (rrsync): it syncs an empty folder over remote's folder,
// deleting only remote.
func (s *rsyncSession) Remove(ctx context.Context, remote string) error {
	empty, err := os.MkdirTemp("", "glovebox-empty-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(empty)
	dir, name := path.Split(remote)
	_, err = s.t.Runner.Run(ctx, "rsync", "-s", "-r", "--delete", "--include=/"+name, "--exclude=*", "--timeout=60",
		"-e", s.rsh(), filepath.ToSlash(empty)+"/", s.t.dest()+":"+path.Join(s.t.Path, dir)+"/")
	return err
}

func (s *rsyncSession) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.t.Runner.Run(ctx, "ssh", "-S", s.sock, "-O", "exit", s.t.dest())
	err := s.proc.Stop()
	if rerr := os.Remove(s.sock); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}
