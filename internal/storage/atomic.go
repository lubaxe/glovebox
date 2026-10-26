package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// WriteFileAtomic replaces name with data so that a power cut leaves either
// the old file or the new one, never an empty or partial one: it writes a
// temporary file next to it, syncs it to the card, renames it into place
// and syncs the directory. Without the syncs XFS can come back from a power
// cut with the rename done but the data not, an empty file.
func WriteFileAtomic(name string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(name)
	f, err := os.CreateTemp(dir, "."+filepath.Base(name)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, name); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil // no directory sync there; only tests run on it
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
