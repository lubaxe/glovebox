//go:build integration

package crypt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/lubaxe/glovebox/internal/system"
)

// TestIntegrationVolume formats, opens, recovers and closes a real LUKS2
// volume in an image file. Run as root on the Pi, with nothing else using
// the device-mapper name "backingfiles":
//
//	GOOS=linux GOARCH=arm64 go test -c -tags integration -o crypttest ./internal/crypt
//	sudo ./crypttest -test.run Integration -test.v
func TestIntegrationVolume(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	r := system.Exec{}
	if _, err := r.Run(ctx, "cryptsetup", "status", Name); err == nil {
		t.Fatalf("/dev/mapper/%s is in use", Name)
	}
	dir, err := os.MkdirTemp("/var/tmp", "glovebox-crypttest-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	img := filepath.Join(dir, "volume.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(512 << 20); err != nil { // XFS needs over 300 MB
		t.Fatal(err)
	}
	f.Close()

	v := &Volume{Device: img, KeyFile: filepath.Join(dir, "crypt", "device.key"), TempDir: filepath.Join(dir, "run"), Runner: r}
	defer r.Run(ctx, "cryptsetup", "close", Name)
	if err := v.Format(ctx, img); err != nil {
		t.Fatal(err)
	}
	if out, err := r.Run(ctx, "cryptsetup", "luksDump", img); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("luksDump:\n%s", out)
	}
	if err := v.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if open, err := v.IsOpen(ctx); err != nil || !open {
		t.Fatalf("IsOpen = %v, %v", open, err)
	}
	if _, err := r.Run(ctx, "mkfs.xfs", "-q", "-f", "-K", "-m", "reflink=1", "-L", "backingfiles", v.Mapper()); err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(dir, "mnt")
	os.Mkdir(mnt, 0o755)
	if _, err := r.Run(ctx, "mount", v.Mapper(), mnt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "hello.txt"), []byte("footage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, "umount", mnt); err != nil {
		t.Fatal(err)
	}

	key, err := v.VolumeKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("volume key: %d bytes", len(key))
	id, _ := age.GenerateX25519Identity()
	rec := Recovery{Dir: filepath.Join(dir, "boot", "glovebox-recovery"), RecipientFile: filepath.Join(dir, "crypt", "recovery.pub")}
	if err := rec.SetRecipient(id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := rec.SaveVolumeKey(id.Recipient().String(), key); err != nil {
		t.Fatal(err)
	}
	if err := v.BackupHeader(ctx, rec.HeaderPath()); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if open, _ := v.IsOpen(ctx); open {
		t.Fatal("still open after Close")
	}

	// The service, unlocked with the user's key while the car never answers.
	s := &Service{Volume: v, Recovery: rec, Log: quietLog(),
		Check: func(context.Context) error { return errors.New("no car here") }}
	if ok, err := s.Encrypted(ctx); !ok || err != nil {
		t.Fatalf("Encrypted = %v, %v", ok, err)
	}
	if err := s.UnlockWithKey(ctx, id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, "mount", "-o", "ro", v.Mapper(), mnt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(mnt, "hello.txt"))
	r.Run(ctx, "umount", mnt)
	if err != nil || string(b) != "footage" {
		t.Fatalf("read back %q, %v", b, err)
	}
	if left, _ := os.ReadDir(v.TempDir); len(left) != 0 {
		t.Errorf("key files left in %s: %v", v.TempDir, left)
	}

	// A restart during the same wake finds it open and doesn't check.
	s2 := &Service{Volume: v, Recovery: rec, Log: quietLog(),
		Check: func(context.Context) error { t.Error("checked for the car"); return nil }}
	s2.Encrypted(ctx)
	if err := s2.Run(ctx); err != nil || s2.Status().UnlockedBy != ByRestart {
		t.Errorf("Run = %v, status %+v", err, s2.Status())
	}
}
