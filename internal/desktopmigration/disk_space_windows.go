//go:build windows

package desktopmigration

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func platformAvailableDiskBytes(path string) (uint64, error) {
	directory, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
