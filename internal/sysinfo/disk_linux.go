package sysinfo

import "golang.org/x/sys/unix"

func diskUsage(path string) (*Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, err
	}
	bsize := uint64(st.Bsize)
	return &Disk{Path: path, Total: st.Blocks * bsize, Free: st.Bavail * bsize}, nil
}
