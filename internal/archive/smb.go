package archive

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// SMB archives to an SMB share, using a built-in client rather than a
// kernel mount, which can hang when Wi-Fi drops mid-copy.
type SMB struct {
	Server   string // host or IP
	Share    string
	Folder   string // optional folder inside the share
	User     string
	Password string
	Domain   string
	// Dialect pins the SMB version (see ParseDialect); 0 negotiates the
	// highest both sides support.
	Dialect uint16
	// Parallel is how many writes are in flight per file. More than one
	// hides Wi-Fi latency; 4 saturates the Pi Zero 2 W's link.
	Parallel int
}

// ParseDialect turns a version like "2.1" or "3.1.1" into its SMB dialect
// number; "" means negotiate.
//
// On the Pi Zero 2 W, which has no AES or SHA instructions, SMB 2.1 is the
// fastest: it signs with HMAC-SHA256, which is cheaper in software than the
// AES-CMAC of SMB 3.x. Without encryption (Samba's default) SMB 3.x adds no
// protection on the wire beyond that.
func ParseDialect(v string) (uint16, error) {
	switch v {
	case "":
		return 0, nil
	case "2.0.2":
		return 0x0202, nil
	case "2.1":
		return 0x0210, nil
	case "3.0":
		return 0x0300, nil
	case "3.0.2":
		return 0x0302, nil
	case "3.1.1":
		return 0x0311, nil
	}
	return 0, fmt.Errorf("unknown SMB version %q: use 2.1, 3.0, 3.0.2 or 3.1.1", v)
}

func (t *SMB) String() string { return fmt.Sprintf("//%s/%s", t.Server, t.Share) }

func (t *SMB) addr() string { return net.JoinHostPort(t.Server, "445") }

// Reachable implements Target.
func (t *SMB) Reachable(ctx context.Context) error {
	return dial(ctx, t.addr())
}

// Connect implements Target.
func (t *SMB) Connect(ctx context.Context) (Session, error) {
	sess, share, err := t.mount(ctx, t.Share)
	if err != nil {
		return nil, err
	}
	return &smbSession{sess: sess, share: share, folder: t.Folder, parallel: max(t.Parallel, 1)}, nil
}

// mount logs in and opens a share.
func (t *SMB) mount(ctx context.Context, name string) (*smb2.Session, *smb2.Share, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", t.addr())
	if err != nil {
		return nil, nil, err
	}
	d := &smb2.Dialer{
		Negotiator: smb2.Negotiator{SpecifiedDialect: t.Dialect},
		Initiator:  &smb2.NTLMInitiator{User: t.User, Password: t.Password, Domain: t.Domain},
	}
	sess, err := d.DialContext(dialCtx, conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("logging in to %s as %s: %w", t.Server, t.User, err)
	}
	share, err := sess.Mount(name)
	if err != nil {
		_ = sess.Logoff()
		return nil, nil, fmt.Errorf("opening share %s: %w", name, err)
	}
	return sess, share, nil
}

// Mirror implements Target. source is "share" or "share/folder".
func (t *SMB) Mirror(ctx context.Context, source, dir string) error {
	name, folder, _ := strings.Cut(strings.Trim(source, "/"), "/")
	if name == "" {
		return fmt.Errorf("no share in %q", source)
	}
	sess, share, err := t.mount(ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		_ = share.Umount()
		_ = sess.Logoff()
	}()
	return mirrorFS(ctx, share.WithContext(ctx).DirFS(folder), dir)
}

type smbSession struct {
	sess     *smb2.Session
	share    *smb2.Share
	folder   string
	parallel int
}

func (s *smbSession) full(remote string) string {
	return path.Join(s.folder, remote)
}

func (s *smbSession) Size(ctx context.Context, remote string) (int64, bool, error) {
	fi, err := s.share.WithContext(ctx).Stat(s.full(remote))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return fi.Size(), true, nil
}

func (s *smbSession) Upload(ctx context.Context, remote string, r io.ReaderAt, size int64) (err error) {
	share := s.share.WithContext(ctx)
	full := s.full(remote)
	if dir := path.Dir(full); dir != "." {
		if err := share.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// Each attempt gets its own temporary name. When the car cuts the power
	// mid-upload, the server can keep the dead connection's file open for
	// hours, and a fixed name would then never be renamed into place.
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	tmp := fmt.Sprintf("%s.%x.partial", full, nonce)
	_ = share.Remove(full + ".partial") // left by versions that used a fixed name
	f, err := share.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			_ = share.Remove(tmp)
		}
	}()
	err = parallelCopy(f, r, size, s.parallel, 1<<20)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", full, err)
	}
	if fi, err := share.Stat(tmp); err != nil {
		return err
	} else if fi.Size() != size {
		return fmt.Errorf("%s is %d bytes on the server, expected %d", full, fi.Size(), size)
	}
	// replace an older, shorter copy
	if err := share.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replacing %s: %w", full, err)
	}
	if err := share.Rename(tmp, full); err != nil {
		return fmt.Errorf("renaming %s: %w", tmp, err)
	}
	return nil
}

func (s *smbSession) Remove(ctx context.Context, remote string) error {
	return s.share.WithContext(ctx).Remove(s.full(remote))
}

func (s *smbSession) Close() error {
	err := s.share.Umount()
	if lerr := s.sess.Logoff(); err == nil {
		err = lerr
	}
	return err
}

// parallelCopy copies size bytes from src to dst in chunks, with up to
// workers writes in flight, so the next request is on the wire while the
// server handles the previous one.
func parallelCopy(dst io.WriterAt, src io.ReaderAt, size int64, workers, chunk int) error {
	offsets := make(chan int64)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			buf := make([]byte, chunk)
			for off := range offsets {
				n := int(min(int64(chunk), size-off))
				// ReadAt may return io.EOF along with a full last chunk
				if m, err := src.ReadAt(buf[:n], off); m < n {
					if err == nil || errors.Is(err, io.EOF) {
						err = io.ErrUnexpectedEOF // the file shrank since it was listed
					}
					errs <- fmt.Errorf("reading at %d: %w", off, err)
					for range offsets { // let the producer finish
					}
					return
				}
				if _, err := dst.WriteAt(buf[:n], off); err != nil {
					errs <- err
					for range offsets {
					}
					return
				}
			}
			errs <- nil
		}()
	}
	for off := int64(0); off < size; off += int64(chunk) {
		offsets <- off
	}
	close(offsets)
	var first error
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ReadCredentials reads a mount.cifs-style credentials file:
//
//	username=carcam
//	password=...
//	domain=WORKGROUP   (optional)
func ReadCredentials(path string) (user, password, domain string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(strings.ToLower(k)) {
		case "username", "user":
			user = strings.TrimSpace(v)
		case "password", "pass":
			password = strings.TrimRight(v, "\r") // keep spaces: they can be part of a password
		case "domain", "workgroup":
			domain = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", "", err
	}
	if user == "" || password == "" {
		return "", "", "", fmt.Errorf("%s needs username= and password= lines", path)
	}
	return user, password, domain, nil
}
