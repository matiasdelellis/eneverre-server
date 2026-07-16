//go:build !windows

package server

import "golang.org/x/sys/unix"

// diskUsage returns the total and available bytes of the filesystem that holds
// path. Available (Bavail) is the space usable by an unprivileged process, which
// is the honest "free" figure for an operator watching recording headroom.
func diskUsage(path string) (total, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, nil
}
