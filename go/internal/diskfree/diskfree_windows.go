//go:build windows

package diskfree

import "golang.org/x/sys/windows"

func statFS(path string, st *statT) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return err
	}
	st.totalBytes = totalBytes
	st.freeToCaller = freeToCaller
	return nil
}
