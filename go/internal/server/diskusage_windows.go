//go:build windows

package server

import "eneverre/internal/diskfree"

// diskUsage returns the total and caller-available bytes of the volume that
// holds path, via GetDiskFreeSpaceEx (the Windows counterpart of statfs). See
// internal/diskfree for the shared implementation.
func diskUsage(path string) (total, free uint64, err error) {
	free, err = diskfree.Available(path)
	if err != nil {
		return 0, 0, err
	}
	total, err = diskfree.Total(path)
	if err != nil {
		return 0, 0, err
	}
	return total, free, nil
}
