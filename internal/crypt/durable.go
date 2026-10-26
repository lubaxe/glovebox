package crypt

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

type syncCloser interface {
	Sync() error
	Close() error
}

func syncAndClose(file syncCloser) error {
	return errors.Join(file.Sync(), file.Close())
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return syncAndClose(dir)
}

func mkdirAllDurable(path string, perm fs.FileMode) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: fs.ErrExist}
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := mkdirAllDurable(parent, perm); err != nil {
		return err
	}
	if err := os.Mkdir(path, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDirectory(parent)
}

func replaceDurable(source, destination string) error {
	return replaceWithSync(source, destination, syncDirectory)
}

func replaceWithSync(source, destination string, syncDir func(string) error) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	err := syncDir(filepath.Dir(destination))
	if filepath.Dir(source) != filepath.Dir(destination) {
		err = errors.Join(err, syncDir(filepath.Dir(source)))
	}
	return err
}

func writeAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := file.Chmod(perm); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := syncAndClose(file); err != nil {
		return err
	}
	return replaceDurable(file.Name(), path)
}
