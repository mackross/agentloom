//go:build windows

package tomlfile

import "os"

func preserveFileOwner(_ *os.File, _ os.FileInfo) error {
	return nil
}
