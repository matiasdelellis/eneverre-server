//go:build !windows

package diskfree

import "golang.org/x/sys/unix"

func statFS(path string, st *statT) error {
	var u unix.Statfs_t
	if err := unix.Statfs(path, &u); err != nil {
		return err
	}
	st.bsize = uint64(u.Bsize)
	st.blocks = u.Blocks
	st.bavail = u.Bavail
	return nil
}
