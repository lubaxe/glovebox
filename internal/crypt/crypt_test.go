package crypt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/lubaxe/glovebox/internal/system"
)

// fakeLUKS answers the cryptsetup and blkid commands a Volume runs.
type fakeLUKS struct {
	t      *testing.T
	mu     sync.Mutex
	exists bool   // a volume labelled Label exists
	open   bool   // it's open
	vk     []byte // its volume key
	// openedWith is the volume key file's contents at the last
	// --volume-key-file open
	openedWith []byte
	failOpen   bool
}

func (f *fakeLUKS) run(cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	arg := func(flag string) string {
		fs := strings.Fields(cmd)
		if i := slices.Index(fs, flag); i >= 0 && i+1 < len(fs) {
			return fs[i+1]
		}
		f.t.Fatalf("%q has no %s", cmd, flag)
		return ""
	}
	switch {
	case cmd == "blkid -L "+Label:
		if !f.exists {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 2}
		}
		return []byte("/dev/mmcblk0p3\n"), nil
	case strings.HasPrefix(cmd, "cryptsetup isLuks "):
		if !f.exists {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 1}
		}
	case cmd == "cryptsetup status "+Name:
		if !f.open {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 4}
		}
	case strings.HasPrefix(cmd, "cryptsetup luksFormat"):
		f.exists, f.vk = true, []byte("0123456789abcdef0123456789abcdef")
	case strings.HasPrefix(cmd, "cryptsetup open"):
		if f.failOpen {
			return nil, &system.CmdError{Cmd: cmd, ExitCode: 2, Output: "No key available with this passphrase."}
		}
		if strings.Contains(cmd, "--volume-key-file") {
			b, err := os.ReadFile(arg("--volume-key-file"))
			if err != nil {
				f.t.Fatal(err)
			}
			f.openedWith = b
		}
		f.open = true
	case strings.HasPrefix(cmd, "cryptsetup luksDump"):
		if err := os.WriteFile(arg("--volume-key-file"), f.vk, 0o600); err != nil {
			f.t.Fatal(err)
		}
	case strings.HasPrefix(cmd, "cryptsetup luksHeaderBackup"):
		if err := os.WriteFile(arg("--header-backup-file"), []byte("header"), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return nil, nil
}

func newVolume(t *testing.T, f *fakeLUKS) (*Volume, *system.FakeRunner) {
	t.Helper()
	f.t = t
	dir := t.TempDir()
	r := &system.FakeRunner{Handler: f.run}
	return &Volume{KeyFile: filepath.Join(dir, "crypt", "device.key"), TempDir: filepath.Join(dir, "run"), Runner: r}, r
}

func newRecovery(t *testing.T) (Recovery, *age.X25519Identity) {
	t.Helper()
	dir := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return Recovery{Dir: filepath.Join(dir, "boot", "glovebox-recovery"), RecipientFile: filepath.Join(dir, "crypt", "recovery.pub")}, id
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFormatCreatesKeyAndUsesAdiantum(t *testing.T) {
	f := &fakeLUKS{}
	v, r := newVolume(t, f)
	if err := v.Format(context.Background(), "/dev/mmcblk0p3"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(v.KeyFile)
	if err != nil || st.Size() != KeySize || st.Mode().Perm() != 0o400 && os.PathSeparator == '/' {
		t.Fatalf("key file: %v %v", st, err)
	}
	want := "cryptsetup luksFormat -q --type luks2 --cipher xchacha12,aes-adiantum-plain64 --key-size 256 --sector-size 4096 " +
		"--pbkdf pbkdf2 --pbkdf-force-iterations 1000 --label glovebox-crypt --key-file " + v.KeyFile + " /dev/mmcblk0p3"
	if got := r.Commands(); len(got) != 1 || got[0] != want {
		t.Errorf("commands = %q\nwant %q", got, want)
	}
	// a second format keeps the key
	key, _ := os.ReadFile(v.KeyFile)
	if err := v.Format(context.Background(), "/dev/mmcblk0p3"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(v.KeyFile); string(again) != string(key) {
		t.Error("the device key changed")
	}
}

func TestEnsureKeyRefusesOddFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "device.key")
	if err := os.WriteFile(p, []byte("short"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := EnsureKey(p); err == nil {
		t.Error("a 5-byte key file was accepted")
	}
}

func TestOpenFindsVolumeByLabel(t *testing.T) {
	f := &fakeLUKS{exists: true}
	v, r := newVolume(t, f)
	if err := v.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "cryptsetup open --type luks2 --allow-discards --key-file " + v.KeyFile + " /dev/mmcblk0p3 backingfiles"
	if got := r.Commands(); got[len(got)-1] != want {
		t.Errorf("last command = %q\nwant %q", got[len(got)-1], want)
	}
	if open, err := v.IsOpen(context.Background()); err != nil || !open {
		t.Errorf("IsOpen = %v, %v", open, err)
	}
}

func TestFindWithoutVolume(t *testing.T) {
	v, _ := newVolume(t, &fakeLUKS{})
	if _, err := v.Find(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find = %v, want ErrNotFound", err)
	}
	v.Device = filepath.Join(t.TempDir(), "missing.img")
	if _, err := v.Find(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find with a missing Device = %v, want ErrNotFound", err)
	}
	// a Device that's still the plain filesystem
	if err := os.WriteFile(v.Device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Find(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find with a plain Device = %v, want ErrNotFound", err)
	}
}

func TestRunKeepsTryingWhenOpenFails(t *testing.T) {
	f := &fakeLUKS{exists: true, failOpen: true}
	s, _ := newService(t, f, func(context.Context) error { return nil })
	s.Encrypted(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if st := s.Status(); st.State != Waiting || !strings.Contains(st.LastError, "opening the drive: ") {
		t.Errorf("status = %+v", st)
	}
}

func TestVolumeKeyRoundTrip(t *testing.T) {
	f := &fakeLUKS{exists: true, vk: []byte("the-volume-key")}
	v, _ := newVolume(t, f)
	key, err := v.VolumeKey(context.Background())
	if err != nil || string(key) != "the-volume-key" {
		t.Fatalf("VolumeKey = %q, %v", key, err)
	}
	if err := v.OpenWithVolumeKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if string(f.openedWith) != "the-volume-key" {
		t.Errorf("opened with %q", f.openedWith)
	}
	if left, _ := os.ReadDir(v.TempDir); len(left) != 0 {
		t.Errorf("key files left behind: %v", left)
	}
}

func TestSealUnseal(t *testing.T) {
	rec, id := newRecovery(t)
	if err := rec.SetRecipient("  " + id.Recipient().String() + "\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := rec.Recipient(); err != nil || got != id.Recipient().String() {
		t.Fatalf("Recipient = %q, %v", got, err)
	}
	if err := rec.SaveVolumeKey(id.Recipient().String(), []byte("vk")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{VolumeKeyFile, ReadmeFile, "recovery.pub"} {
		if _, err := os.Stat(filepath.Join(rec.Dir, name)); err != nil {
			t.Error(err)
		}
	}
	// a key file with comments, as age-keygen and the web page write it
	file := "# created: today\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	if key, err := rec.VolumeKey(file); err != nil || string(key) != "vk" {
		t.Errorf("VolumeKey = %q, %v", key, err)
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := rec.VolumeKey(other.String()); !errors.Is(err, ErrWrongKey) {
		t.Errorf("another key: %v, want ErrWrongKey", err)
	}
	if _, err := rec.VolumeKey("hello"); err == nil {
		t.Error("garbage was accepted as a key")
	}
	if _, err := ParseRecipient("age1notakey"); err == nil {
		t.Error("a bad public key was accepted")
	}
}

func newService(t *testing.T, f *fakeLUKS, check func(context.Context) error) (*Service, *age.X25519Identity) {
	t.Helper()
	v, _ := newVolume(t, f)
	rec, id := newRecovery(t)
	dir := t.TempDir()
	return &Service{
		Volume: v, Recovery: rec, Check: check, Log: quietLog(),
		RequestFile: filepath.Join(dir, "encrypt-requested"), ErrorFile: filepath.Join(dir, "encrypt-error"),
		Backoff: []time.Duration{time.Millisecond},
	}, id
}

func TestRunAlreadyOpen(t *testing.T) {
	s, _ := newService(t, &fakeLUKS{exists: true, open: true}, func(context.Context) error {
		t.Error("checked for the car although the drive was open")
		return nil
	})
	if ok, err := s.Encrypted(context.Background()); !ok || err != nil {
		t.Fatalf("Encrypted = %v, %v", ok, err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.State != Unlocked || st.UnlockedBy != ByRestart {
		t.Errorf("status = %+v", st)
	}
}

func TestRunRetriesUntilCarSeen(t *testing.T) {
	f := &fakeLUKS{exists: true}
	calls := 0
	s, _ := newService(t, f, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("the car didn't answer over Bluetooth")
		}
		return nil
	})
	s.Encrypted(context.Background())
	if st := s.Status(); st.State != Waiting {
		t.Fatalf("state before Run = %q", st.State)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.State != Unlocked || st.UnlockedBy != ByCar || st.Attempts != 2 || !f.open || calls != 3 {
		t.Errorf("status = %+v, open %v, checks %d", st, f.open, calls)
	}
	select {
	case <-s.Unlocked():
	default:
		t.Error("Unlocked not closed")
	}
}

func TestRunEnforceStaysLocked(t *testing.T) {
	f := &fakeLUKS{exists: true}
	s, _ := newService(t, f, nil) // no car paired
	s.Encrypted(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if st := s.Status(); st.State != Waiting || f.open || st.Attempts == 0 || st.LastError != ErrNoCar.Error() {
		t.Errorf("status = %+v, open %v", st, f.open)
	}
}

func TestRunAuditOpensAnyway(t *testing.T) {
	f := &fakeLUKS{exists: true}
	s, _ := newService(t, f, func(context.Context) error { return errors.New("no answer") })
	s.Mode = Audit
	s.Encrypted(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.UnlockedBy != ByAudit || !f.open || st.CarCheck != Audit {
		t.Errorf("status = %+v", st)
	}
}

func TestUnlockWithKeyWhileWaiting(t *testing.T) {
	f := &fakeLUKS{exists: true, vk: []byte("vk-bytes")}
	s, id := newService(t, f, func(context.Context) error { return errors.New("no answer") })
	if err := s.Recovery.SetRecipient(id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := s.Recovery.SaveVolumeKey(id.Recipient().String(), f.vk); err != nil {
		t.Fatal(err)
	}
	s.Encrypted(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(context.Background()) }()

	other, _ := age.GenerateX25519Identity()
	if err := s.UnlockWithKey(context.Background(), other.String()); !errors.Is(err, ErrWrongKey) {
		t.Errorf("wrong key: %v", err)
	}
	if err := s.UnlockWithKey(context.Background(), id.String()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if st := s.Status(); st.UnlockedBy != ByKey || string(f.openedWith) != "vk-bytes" {
		t.Errorf("status = %+v, opened with %q", st, f.openedWith)
	}
}

func TestSetRecipientNeedsUnlockedDrive(t *testing.T) {
	f := &fakeLUKS{exists: true, vk: []byte("vk")}
	s, id := newService(t, f, nil)
	s.Encrypted(context.Background())
	if err := s.SetRecipient(context.Background(), id.Recipient().String()); err == nil {
		t.Fatal("changed the key on a locked drive")
	}
	s.markUnlocked(ByKey, 0)
	if err := s.SetRecipient(context.Background(), id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if key, err := s.Recovery.VolumeKey(id.String()); err != nil || string(key) != "vk" {
		t.Errorf("resealed key = %q, %v", key, err)
	}
}

func TestRequestEncrypt(t *testing.T) {
	s, id := newService(t, &fakeLUKS{}, nil)
	s.Encrypted(context.Background())
	if err := s.RequestEncrypt(); err == nil {
		t.Error("encrypting without a recovery key was allowed")
	}
	if err := s.SetRecipient(context.Background(), id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	s.RecordEncryptError(errors.New("old failure"))
	if err := s.RequestEncrypt(); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); !st.EncryptRequested || st.EncryptError != "" || st.Encrypted {
		t.Errorf("status = %+v", st)
	}
	if !s.TakeRequest() || s.TakeRequest() {
		t.Error("TakeRequest should report the request once")
	}
}

func TestConversion(t *testing.T) {
	f := &fakeLUKS{}
	v, r := newVolume(t, f)
	rec, id := newRecovery(t)
	if err := rec.SetRecipient(id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mnt := filepath.ToSlash(filepath.Join(dir, "backingfiles")) // Run creates it
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("proc /proc proc defaults 0 0\nLABEL=backingfiles "+mnt+" xfs noatime,nofail 0 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	released := false
	part := filepath.Join(dir, "mmcblk0p3") // stands in for the partition
	if err := os.WriteFile(part, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.Handler = func(cmd string) ([]byte, error) {
		switch {
		case cmd == "findmnt -n -o SOURCE "+mnt:
			return []byte(part + "\n"), nil
		case cmd == "lsblk -n -d -o TYPE "+part:
			return []byte("part\n"), nil
		case cmd == "findmnt -rn -o TARGET":
			return []byte("/\n" + mnt + "\n/run/glovebox/snapshots/snap-000001\n/run/glovebox/snapshots/snap-000002\n"), nil
		case strings.HasPrefix(cmd, "losetup -n -l"):
			return []byte("/dev/loop0 " + mnt + "/snapshots/snap-000001/snap.bin\n/dev/loop9 /var/lib/other.img\n"), nil
		}
		return f.run(cmd)
	}
	c := &Conversion{
		Mountpoint: mnt, Fstab: fstab, SnapshotRoot: "/run/glovebox/snapshots",
		Volume: v, Recovery: rec, Runner: r,
		Release: func(context.Context) error { released = true; return nil },
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Error("the drive wasn't released first")
	}
	var got []string
	for _, cmd := range r.Commands() {
		if !strings.HasPrefix(cmd, "cryptsetup isLuks") { // checked before each use of the volume
			got = append(got, strings.Join(strings.Fields(cmd)[:2], " "))
		}
	}
	want := []string{
		"findmnt -n", "lsblk -n",
		"findmnt -rn", "umount /run/glovebox/snapshots/snap-000001", "umount /run/glovebox/snapshots/snap-000002",
		"losetup -n", "losetup -d",
		"mountpoint -q", "umount " + mnt,
		"wipefs -a", "cryptsetup luksFormat", "cryptsetup open", "cryptsetup luksDump", "cryptsetup luksHeaderBackup",
		"mkfs.xfs -f", "mount -t",
	}
	if !slices.Equal(got, want) {
		t.Errorf("commands:\n got %q\nwant %q", got, want)
	}
	if b, _ := os.ReadFile(fstab); strings.Contains(string(b), mnt) || !strings.Contains(string(b), "proc") {
		t.Errorf("fstab = %q", b)
	}
	if key, err := rec.VolumeKey(id.String()); err != nil || string(key) != string(f.vk) {
		t.Errorf("sealed key = %q, %v", key, err)
	}
	if _, err := os.Stat(rec.HeaderPath()); err != nil {
		t.Error(err)
	}
}

func TestConversionRefusesLoopFile(t *testing.T) {
	v, r := newVolume(t, &fakeLUKS{})
	rec, id := newRecovery(t)
	rec.SetRecipient(id.Recipient().String())
	r.Handler = func(cmd string) ([]byte, error) {
		switch {
		case strings.HasPrefix(cmd, "findmnt -n -o SOURCE"):
			return []byte("/dev/loop0\n"), nil
		case strings.HasPrefix(cmd, "lsblk"):
			return []byte("loop\n"), nil
		}
		t.Errorf("ran %q", cmd)
		return nil, nil
	}
	c := &Conversion{Mountpoint: "/backingfiles", Volume: v, Recovery: rec, Runner: r}
	if err := c.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "only a partition") {
		t.Errorf("Run = %v", err)
	}
}

func TestConversionNeedsRecipient(t *testing.T) {
	v, r := newVolume(t, &fakeLUKS{})
	rec, _ := newRecovery(t)
	c := &Conversion{Mountpoint: "/backingfiles", Volume: v, Recovery: rec, Runner: r}
	if err := c.Run(context.Background()); err == nil || len(r.Commands()) != 0 {
		t.Errorf("Run = %v after %q", err, r.Commands())
	}
}

func TestRunAfterCreateKeepsHow(t *testing.T) {
	s, _ := newService(t, &fakeLUKS{exists: true, open: true}, nil)
	s.Created()
	s.Encrypted(context.Background())
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.UnlockedBy != ByCreate {
		t.Errorf("unlocked by %q, want %q", st.UnlockedBy, ByCreate)
	}
}
