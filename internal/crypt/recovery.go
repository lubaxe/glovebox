package crypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/lubaxe/glovebox/internal/bootfs"
)

// Recovery keeps what the user's own key pair needs to recover the volume:
// the public key on the Pi, and the sealed volume key and a header backup
// on the boot partition, where a PC can read them from the SD card.
type Recovery struct {
	// Dir is the folder on the boot partition, e.g.
	// /boot/firmware/glovebox-recovery.
	Dir string
	// RecipientFile holds the user's age public key, e.g.
	// /etc/glovebox/crypt/recovery.pub.
	RecipientFile string
	BootWriter    bootfs.Writer
}

// Files in Recovery.Dir.
const (
	VolumeKeyFile = "volume-key.age"
	HeaderFile    = "backingfiles-header.img"
	ReadmeFile    = "README.txt"
)

// HeaderPath is where the header backup goes.
func (r Recovery) HeaderPath() string { return filepath.Join(r.Dir, HeaderFile) }

// ParseRecipient checks an age public key ("age1…") and returns it trimmed.
func ParseRecipient(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, err := age.ParseX25519Recipient(s); err != nil {
		return "", fmt.Errorf("that isn't an age public key (it should start with age1): %w", err)
	}
	return s, nil
}

// Recipient returns the user's public key, or "" if none is set.
func (r Recovery) Recipient() (string, error) {
	b, err := os.ReadFile(r.RecipientFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ParseRecipient(string(b))
}

// SetRecipient stores the user's public key, on the Pi and, for reference,
// next to the sealed key on the boot partition.
func (r Recovery) SetRecipient(recipient string) error {
	return bootfs.WithWrite(context.Background(), r.BootWriter, func() error {
		return r.setRecipient(recipient)
	})
}

func (r Recovery) setRecipient(recipient string) error {
	recipient, err := ParseRecipient(recipient)
	if err != nil {
		return err
	}
	if err := mkdirAllDurable(filepath.Dir(r.RecipientFile), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(r.RecipientFile, []byte(recipient+"\n"), 0o644); err != nil {
		return err
	}
	if err := mkdirAllDurable(r.Dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.Dir, "recovery.pub"), []byte(recipient+"\n"), 0o644)
}

// SaveVolumeKey seals the volume key to the user's public key and writes
// it, with the README, to Dir.
func (r Recovery) SaveVolumeKey(recipient string, key []byte) error {
	return bootfs.WithWrite(context.Background(), r.BootWriter, func() error {
		return r.saveVolumeKey(recipient, key)
	})
}

func (r Recovery) saveVolumeKey(recipient string, key []byte) error {
	sealed, err := Seal(recipient, key)
	if err != nil {
		return err
	}
	if err := mkdirAllDurable(r.Dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(r.Dir, VolumeKeyFile), sealed, 0o644); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.Dir, ReadmeFile), []byte(readme), 0o644)
}

func (r Recovery) preflight() (err error) {
	if err := mkdirAllDurable(r.Dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{VolumeKeyFile, HeaderFile, ReadmeFile} {
		info, err := os.Lstat(filepath.Join(r.Dir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("recovery output %s is not a regular file", name)
		}
	}
	dir, err := os.MkdirTemp(r.Dir, ".recovery-preflight-*")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir), syncDirectory(r.Dir))
	}()
	probe := filepath.Join(dir, "probe")
	if err := writeAtomic(probe, []byte("recovery write check\n"), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(probe, []byte("recovery replace check\n"), 0o600); err != nil {
		return err
	}
	return nil
}

// VolumeKey unseals the volume key with the user's private key: the
// contents of their key file, or just its AGE-SECRET-KEY-1… line.
func (r Recovery) VolumeKey(identity string) ([]byte, error) {
	sealed, err := os.ReadFile(filepath.Join(r.Dir, VolumeKeyFile))
	if err != nil {
		return nil, fmt.Errorf("reading the sealed volume key: %w", err)
	}
	return Unseal(identity, sealed)
}

// Seal encrypts data to an age public key, ASCII-armored.
func Seal(recipient string, data []byte) ([]byte, error) {
	rcpt, err := age.ParseX25519Recipient(strings.TrimSpace(recipient))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, rcpt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrWrongKey means the private key doesn't open what was sealed.
var ErrWrongKey = errors.New("that key doesn't open this drive")

// Unseal decrypts what Seal made with the matching private key.
func Unseal(identity string, sealed []byte) ([]byte, error) {
	ids, err := age.ParseIdentities(strings.NewReader(identity))
	if err != nil {
		return nil, errors.New("that isn't an age private key (it should contain a line starting with AGE-SECRET-KEY-1)")
	}
	rd, err := age.Decrypt(armor.NewReader(bytes.NewReader(sealed)), ids...)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, ErrWrongKey
		}
		return nil, err
	}
	return io.ReadAll(rd)
}

const readme = `Glovebox drive encryption: recovery
=====================================

The third partition on this card holds Glovebox's footage in an encrypted
(LUKS2) volume. Glovebox opens it by itself once it has seen its car over
Bluetooth. Without the Pi and the car, your own key opens it:

- volume-key.age           the volume's master key, encrypted to your age
                           public key (recovery.pub). Only your private key
                           (AGE-SECRET-KEY-1...) can decrypt it.
- backingfiles-header.img  a backup of the volume's header. It's useless
                           without a key; it repairs a damaged header.

On the Pi: open the Glovebox web page and choose "Unlock with your key".

On a Linux PC (or WSL on Windows), with the card in a reader at /dev/sdX:

    age -d -i your-key.txt volume-key.age > /tmp/vk
    sudo cryptsetup open --volume-key-file /tmp/vk /dev/sdX3 glovebox
    shred -u /tmp/vk
    sudo mount -o ro /dev/mapper/glovebox /mnt
    # the car's drive is /mnt/cam_disk.bin, a disk image; to see the clips:
    loop=$(sudo losetup -f -P -r --show /mnt/cam_disk.bin)
    sudo mount -o ro "${loop}p1" /media/cam

If the header is damaged:

    sudo cryptsetup luksHeaderRestore /dev/sdX3 --header-backup-file backingfiles-header.img
`
