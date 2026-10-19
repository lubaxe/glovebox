// Package crypt keeps backingfiles (the car's drive, snapshots, archive
// state and log) in a LUKS2 volume on the Pi.
//
// The volume opens with a random device key kept on the Pi, so the car's
// drive can be presented without anyone typing anything. Glovebox only
// opens it once it has seen its own car over Bluetooth (see Service), and
// it stays open until the power is cut. The volume's master key is also
// sealed to the user's own age key pair, whose private half never needs to
// be on the Pi: with it the footage can be recovered on the Pi or on a PC.
//
// Adiantum is used because the Pi Zero 2 W's CPU has no AES instructions;
// it runs at over 100 MiB/s there, well above what the SD card can do.
package crypt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubaxe/glovebox/internal/system"
)

const (
	// Label is the LUKS2 label of the encrypted partition. It isn't
	// "backingfiles", the XFS label inside, so that blkid -L finds each one
	// unambiguously.
	Label = "glovebox-crypt"
	// Name is the device-mapper name: the open volume is /dev/mapper/backingfiles.
	Name = "backingfiles"
	// Cipher is Adiantum, made for CPUs without AES instructions.
	Cipher = "xchacha12,aes-adiantum-plain64"
	// KeySize is the device key file's size in bytes.
	KeySize = 64
)

// Volume is the encrypted volume holding backingfiles.
type Volume struct {
	// Device is the partition (or image file) holding the volume; empty
	// means the one labelled Label.
	Device string
	// KeyFile holds the device key.
	KeyFile string
	// TempDir is where the volume key is written for a moment while
	// cryptsetup reads it; it should be a tmpfs such as /run/glovebox.
	TempDir string
	Runner  system.Runner
}

// Mapper is the open volume's device.
func (v *Volume) Mapper() string { return "/dev/mapper/" + Name }

// ErrNotFound means there is no encrypted volume.
var ErrNotFound = errors.New("no encrypted volume")

// Find returns the volume's device, or ErrNotFound if there's none.
func (v *Volume) Find(ctx context.Context) (string, error) {
	if v.Device != "" {
		if _, err := os.Stat(v.Device); err != nil {
			return "", fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		// not encrypted yet: still the plain filesystem
		if _, err := v.Runner.Run(ctx, "cryptsetup", "isLuks", v.Device); system.ExitCode(err) > 0 {
			return "", ErrNotFound
		} else if err != nil {
			return "", err
		}
		return v.Device, nil
	}
	out, err := v.Runner.Run(ctx, "blkid", "-L", Label)
	if system.ExitCode(err) == 2 {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", ErrNotFound
	}
	return dev, nil
}

// IsOpen reports whether the volume is open (until the power is cut, or
// Close).
func (v *Volume) IsOpen(ctx context.Context) (bool, error) {
	_, err := v.Runner.Run(ctx, "cryptsetup", "status", Name)
	if err == nil {
		return true, nil
	}
	if system.ExitCode(err) > 0 {
		return false, nil // 4: not active
	}
	return false, err
}

// Open opens the volume with the device key.
func (v *Volume) Open(ctx context.Context) error {
	dev, err := v.Find(ctx)
	if err != nil {
		return err
	}
	_, err = v.Runner.Run(ctx, "cryptsetup", "open", "--type", "luks2", "--allow-discards",
		"--key-file", v.KeyFile, dev, Name)
	return err
}

// OpenWithVolumeKey opens the volume with its master key, as recovered with
// the user's key.
func (v *Volume) OpenWithVolumeKey(ctx context.Context, key []byte) error {
	dev, err := v.Find(ctx)
	if err != nil {
		return err
	}
	f, err := v.tempFile(key)
	if err != nil {
		return err
	}
	defer wipe(f)
	_, err = v.Runner.Run(ctx, "cryptsetup", "open", "--type", "luks2", "--allow-discards",
		"--volume-key-file", f, dev, Name)
	return err
}

// Close closes the volume; it must be unmounted first.
func (v *Volume) Close(ctx context.Context) error {
	_, err := v.Runner.Run(ctx, "cryptsetup", "close", Name)
	return err
}

// Format makes dev a new, empty encrypted volume opened by the device key,
// creating the key if needed. Whatever was on dev is lost.
func (v *Volume) Format(ctx context.Context, dev string) error {
	if err := EnsureKey(v.KeyFile); err != nil {
		return err
	}
	// The key is random, so a slow key derivation buys nothing and would
	// only delay the car's drive at every start.
	_, err := v.Runner.Run(ctx, "cryptsetup", "luksFormat", "-q", "--type", "luks2",
		"--cipher", Cipher, "--key-size", "256", "--sector-size", "4096",
		"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--label", Label, "--key-file", v.KeyFile, dev)
	if err != nil {
		return fmt.Errorf("formatting %s: %w", dev, err)
	}
	return nil
}

// VolumeKey returns the volume's master key, read with the device key.
func (v *Volume) VolumeKey(ctx context.Context) ([]byte, error) {
	dev, err := v.Find(ctx)
	if err != nil {
		return nil, err
	}
	f, err := v.tempFile(nil)
	if err != nil {
		return nil, err
	}
	defer wipe(f)
	if err := os.Remove(f); err != nil { // cryptsetup creates it
		return nil, err
	}
	if _, err := v.Runner.Run(ctx, "cryptsetup", "luksDump", "-q", "--dump-volume-key",
		"--volume-key-file", f, "--key-file", v.KeyFile, dev); err != nil {
		return nil, fmt.Errorf("reading the volume key: %w", err)
	}
	key, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, errors.New("cryptsetup wrote an empty volume key")
	}
	return key, nil
}

// BackupHeader saves the volume's header to path, replacing it. The header
// is useless without a key, and restores a volume whose header got damaged.
func (v *Volume) BackupHeader(ctx context.Context, path string) error {
	dev, err := v.Find(ctx)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err = v.Runner.Run(ctx, "cryptsetup", "luksHeaderBackup", dev, "--header-backup-file", path)
	return err
}

// tempFile writes data to a new private file in TempDir.
func (v *Volume) tempFile(data []byte) (string, error) {
	dir := v.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "volume-key-")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// wipe overwrites a key file before removing it.
func wipe(path string) {
	if st, err := os.Stat(path); err == nil {
		_ = os.WriteFile(path, make([]byte, st.Size()), 0o600)
	}
	_ = os.Remove(path)
}

// EnsureKey creates the device key file, KeySize random bytes readable only
// by root, unless it already exists.
func EnsureKey(path string) error {
	if st, err := os.Stat(path); err == nil {
		if st.Size() != KeySize {
			return fmt.Errorf("%s is %d bytes; a device key is %d", path, st.Size(), KeySize)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, key, 0o400); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
