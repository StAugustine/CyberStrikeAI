//go:build !windows

package desktopplugin

import (
	"errors"
	"os"
)

func validateDiscoveryFilePermissions(info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("desktop discovery permissions are too broad")
	}
	return nil
}
