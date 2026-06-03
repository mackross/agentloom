package filetool

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// PathRestrictionConfig restricts file tools to a configured set of glob
// patterns. A nil *PathRestrictionConfig means unrestricted access. When
// configured, paths must stay within CWD and match at least one
// RestrictToGlobs pattern. Patterns use doublestar syntax and are matched
// against slash-separated paths relative to CWD.
type PathRestrictionConfig struct {
	RestrictToGlobs []string
}

func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if cwd == "" {
		cwd = "."
	}
	return filepath.Join(cwd, path)
}

func checkPathAllowed(cwd, path string, restrictions *PathRestrictionConfig) error {
	if restrictions == nil {
		return nil
	}
	display, err := restrictedDisplayPath(cwd, path)
	if err != nil {
		return accessDeniedError(path)
	}
	for _, pattern := range restrictions.RestrictToGlobs {
		pattern = filepath.ToSlash(filepath.Clean(pattern))
		if pattern == "." {
			continue
		}
		ok, err := doublestar.Match(pattern, display)
		if err != nil {
			return fmt.Errorf("invalid path restriction glob %q: %w", pattern, err)
		}
		if ok {
			return nil
		}
	}
	return accessDeniedError(display)
}

func restrictedDisplayPath(cwd, path string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	full, err = filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", accessDeniedError(path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", accessDeniedError(path)
	}
	return filepath.ToSlash(rel), nil
}

func accessDeniedError(path string) error {
	return fmt.Errorf("access denied: %s", path)
}
