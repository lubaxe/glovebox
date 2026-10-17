//go:build !linux

package sysinfo

import "errors"

func diskUsage(string) (*Disk, error) {
	return nil, errors.ErrUnsupported
}
