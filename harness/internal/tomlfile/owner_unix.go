//go:build !windows

package tomlfile

import (
	"fmt"
	"os"
	"syscall"
)

func preserveFileOwner(dst *os.File, src os.FileInfo) error {
	stat, ok := src.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unsupported file stat type %T", src.Sys())
	}
	return dst.Chown(int(stat.Uid), int(stat.Gid))
}
