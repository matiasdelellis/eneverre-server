//go:build windows

package server

import "golang.org/x/sys/windows"

// diskUsage returns the total and caller-available bytes of the volume that
// holds path, via GetDiskFreeSpaceEx (the Windows counterpart of statfs).
func diskUsage(path string) (total, free uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return totalBytes, freeToCaller, nil
}
