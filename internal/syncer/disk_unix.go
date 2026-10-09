//go:build linux || darwin

package syncer

import "syscall"

// availableBytes reports the free space available to unprivileged users on the
// filesystem holding path, and whether it could be determined.
func availableBytes(path string) (uint64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false
	}
	// Bavail rather than Bfree: reserved blocks are not available to us, and
	// counting them would promise space the write cannot use.
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
