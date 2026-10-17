package keepawake

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureKey(t *testing.T) {
	priv := filepath.Join(t.TempDir(), "ble", "private.pem")
	created, err := EnsureKey(priv)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	privPEM, _ := os.ReadFile(priv)
	block, _ := pem.Decode(privPEM)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := os.ReadFile(filepath.Join(filepath.Dir(priv), "public.pem"))
	block, _ = pem.Decode(pubPEM)
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil || !key.PublicKey.Equal(pub.(*ecdsa.PublicKey)) {
		t.Errorf("public key doesn't match: %v", err)
	}
	if created, err := EnsureKey(priv); created || err != nil {
		t.Errorf("second call: created=%v err=%v", created, err)
	}
	if again, _ := os.ReadFile(priv); string(again) != string(privPEM) {
		t.Error("an existing key must never be replaced")
	}
}
