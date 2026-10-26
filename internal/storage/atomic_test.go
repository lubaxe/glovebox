package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "state.json")
	for _, data := range []string{"first", "second, longer"} {
		if err := WriteFileAtomic(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(name); err != nil || string(got) != data {
			t.Fatalf("read %q, %v; want %q", got, err, data)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "missing", "x"), nil, 0o644); err == nil {
		t.Error("no error for a missing directory")
	}
}
