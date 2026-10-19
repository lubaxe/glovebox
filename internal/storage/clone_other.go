//go:build !linux

package storage

import "os"

// Clone is only supported on Linux.
func Clone(src, dst string) error {
	return ErrNoReflink
}

// CloneRange is only supported on Linux.
func CloneRange(dst, src *os.File, srcOff, length, dstOff int64) error {
	return ErrNoReflink
}
