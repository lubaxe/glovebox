//go:build !linux

package storage

// Clone is only supported on Linux.
func Clone(src, dst string) error {
	return ErrNoReflink
}
