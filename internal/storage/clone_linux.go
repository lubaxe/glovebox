package storage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Clone makes dst a copy-on-write clone of src (like cp --reflink=always).
// It's instant and takes no extra space until either file changes. dst must
// not exist.
func Clone(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("%w: %v", ErrNoReflink, err)
		}
		return fmt.Errorf("cloning %s: %w", src, err)
	}
	return nil
}
