package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve normalizes user-provided paths against cwd.
func Resolve(cwd, p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "@")

	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}

	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		cwd = "."
	}
	return filepath.Clean(filepath.Join(cwd, p))
}
