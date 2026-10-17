package storage

import "golang.org/x/sys/unix"

// DiskSpace returns the size of the filesystem holding path and the space
// available on it to unprivileged users.
func DiskSpace(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize, nil
}

// FreeBytes returns the space available to unprivileged users on the
// filesystem holding path.
func FreeBytes(path string) (int64, error) {
	_, free, err := DiskSpace(path)
	return free, err
}
