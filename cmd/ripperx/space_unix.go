//go:build unix

package main

import "golang.org/x/sys/unix"

// diskSpace asks the filesystem how much room is left where images are
// written. It is in a file of its own because statfs is not a thing every
// system has, and the rest of ripperX builds where it is not.
func diskSpace(dir string) (free, total int64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	// Bavail rather than Bfree: the blocks reserved for root are not space
	// a rip can use, and counting them means promising room that is not
	// there.
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}
