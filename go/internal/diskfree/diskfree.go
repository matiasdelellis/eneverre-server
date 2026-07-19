// Package diskfree reports the available bytes on the filesystem that holds a
// path. It is a thin, package-build-split wrapper around the platform statfs
// (unix) or GetDiskFreeSpaceEx (Windows) so callers don't each carry their own
// build-tag pair. Available bytes is the caller-usable figure (Bavail on unix,
// free-to-caller on Windows), which is the honest "free" for an unprivileged
// process watching recording headroom.
package diskfree

// Available returns the bytes available to the current process on the
// filesystem that holds path. Returns the underlying syscall error when statfs
// fails; callers typically treat that as "headroom unknown" rather than fatal.
func Available(path string) (uint64, error) {
	var st statT
	if err := statFS(path, &st); err != nil {
		return 0, err
	}
	return st.availableBytes(), nil
}

// Total returns the total size in bytes of the filesystem that holds path.
// Same error semantics as Available.
func Total(path string) (uint64, error) {
	var st statT
	if err := statFS(path, &st); err != nil {
		return 0, err
	}
	return st.totalBytesFn(), nil
}

// statT is the per-platform struct populated by statFS. Implementations live
// in diskfree_unix.go and diskfree_windows.go.
type statT struct {
	bsize        uint64
	blocks       uint64 // unix: total blocks on the filesystem
	bavail       uint64 // unix: free blocks usable by an unprivileged caller
	freeToCaller uint64 // windows: FreeBytesAvailable to the caller
	totalBytes   uint64 // windows: total bytes on the volume
}

func (s *statT) availableBytes() uint64 {
	// On Windows the caller-available figure is already in bytes; on unix
	// we multiply Bavail (free blocks for an unprivileged caller) by the
	// block size, which is the same notion of "available to me".
	if s.freeToCaller != 0 {
		return s.freeToCaller
	}
	return s.bavail * s.bsize
}

func (s *statT) totalBytesFn() uint64 {
	if s.totalBytes != 0 {
		return s.totalBytes
	}
	return s.blocks * s.bsize
}
