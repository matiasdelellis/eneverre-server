//go:build !windows

package server

import "eneverre/internal/diskfree"

// diskUsage returns the total and available bytes of the filesystem that holds
// path. Wraps internal/diskfree, the shared statfs helper used by both this
// package (the /api/status snapshot) and the media engine (low-disk watcher).
// Available (Bavail) is the space usable by an unprivileged process, which is
// the honest "free" figure for an operator watching recording headroom.
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
