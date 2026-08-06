//go:build !windows

package desktopmigration

import "golang.org/x/sys/unix"

func platformAvailableDiskBytes(path string) (uint64, error) {
	var statistics unix.Statfs_t
	if err := unix.Statfs(path, &statistics); err != nil {
		return 0, err
	}
	return saturatingMultiply(uint64(statistics.Bavail), uint64(statistics.Bsize)), nil
}
