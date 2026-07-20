//go:build linux || darwin

package main

import "syscall"

// diskStats returns the total and free byte counts of the filesystem
// containing dir, via syscall.Statfs. This build is used on linux and
// darwin; windows (also a real cross-compile target — see
// cameraui.config.ts's go.targets) has no Statfs syscall, so
// diskstats_windows.go provides the same diskStats signature there via
// golang.org/x/sys/windows.GetDiskFreeSpaceEx instead.
//
// syscall.Statfs_t's Bsize field is int64 on linux and uint32 on darwin;
// casting through uint64 (rather than referencing the field's native type
// directly) is what lets this single file build unmodified for both GOOS
// values under the same build tag.
func diskStats(dir string) (totalBytes, freeBytes uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	return total, free, nil
}
