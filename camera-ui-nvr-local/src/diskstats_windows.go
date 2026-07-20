//go:build windows

package main

import "golang.org/x/sys/windows"

// diskStats returns the total and free byte counts of the volume containing
// dir, via GetDiskFreeSpaceEx. See diskstats_unix.go for the linux/darwin
// equivalent (syscall.Statfs, which does not exist on windows) — both
// files expose the identical diskStats(dir string) (uint64, uint64, error)
// signature so callers (rpc_recording.go's GetStorageStats) need no
// platform-specific code of their own.
func diskStats(dir string) (totalBytes, freeBytes uint64, err error) {
	ptr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}

	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeBytesAvailable, &totalNumberOfBytes, &totalNumberOfFreeBytes); err != nil {
		return 0, 0, err
	}
	return totalNumberOfBytes, freeBytesAvailable, nil
}
