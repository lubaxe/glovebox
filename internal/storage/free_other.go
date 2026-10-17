//go:build !linux

package storage

import "errors"

// DiskSpace is only supported on Linux.
func DiskSpace(string) (total, free int64, err error) {
	return 0, 0, errors.ErrUnsupported
}

// FreeBytes is only supported on Linux.
func FreeBytes(string) (int64, error) {
	return 0, errors.ErrUnsupported
}
