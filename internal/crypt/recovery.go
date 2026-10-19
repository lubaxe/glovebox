package crypt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
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
	recipient, err := ParseRecipient(recipient)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.RecipientFile), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(r.RecipientFile, []byte(recipient+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.Dir, "recovery.pub"), []byte(recipient+"\n"), 0o644)
}

// SaveVolumeKey seals the volume key to the user's public key and writes
// it, with the README, to Dir.
func (r Recovery) SaveVolumeKey(recipient string, key []byte) error {
	sealed, err := Seal(recipient, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(r.Dir, VolumeKeyFile), sealed, 0o644); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.Dir, ReadmeFile), []byte(readme), 0o644)
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

func writeAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
