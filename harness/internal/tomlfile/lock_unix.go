//go:build !windows

package tomlfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const lockRetryInterval = 25 * time.Millisecond

func lockFile(ctx context.Context, f *os.File, exclusive bool) error {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock toml file: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock toml file: %w", ctx.Err())
		case <-time.After(lockRetryInterval):
		}
	}
}

func unlockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock toml file: %w", err)
	}
	return nil
}
