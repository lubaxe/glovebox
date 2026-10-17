package keepawake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// EnsureKey creates the Bluetooth key the car is paired with, if it doesn't
// exist yet: a P-256 private key at privPath (root-only) and its public half
// at public.pem next to it, which is what gets paired. It reports whether
// it created them.
func EnsureKey(privPath string) (created bool, err error) {
	if _, err := os.Stat(privPath); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return false, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(privPath), 0o700); err != nil {
		return false, err
	}
	pubPath := filepath.Join(filepath.Dir(privPath), "public.pem")
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o644); err != nil {
		return false, err
	}
	// written last, so a half-made key is redone next time
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
