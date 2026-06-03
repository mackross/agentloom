package filetool

import "path/filepath"

func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if cwd == "" {
		cwd = "."
	}
	return filepath.Join(cwd, path)
}
