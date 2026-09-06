//go:build windows

package tomlfile

import (
	"context"
	"os"
)

func lockFile(ctx context.Context, f *os.File, exclusive bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func unlockFile(f *os.File) error { return nil }
