//go:build windows

package desktopplugin

import "os"

func validateDiscoveryFilePermissions(_ os.FileInfo) error {
	return nil
}
