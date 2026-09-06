// Package tomlfile provides locked, path-addressable transactions over TOML files.
package tomlfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const defaultLockTimeout = 10 * time.Second

// Handle is the mutable TOML handle passed to Tx callbacks.
//
// Values are addressed by dotted TOML path, for example "openai.fast" or
// "openai.codex.access_token". A write transaction loads the latest file from
// disk while holding the file lock, invokes the callback, then writes the
// mutated document atomically if the callback changed it.
type Handle struct {
	data    map[string]any
	changed bool
}

// Tx runs fn with a mutable TOML handle for path.
func Tx(path string, fn func(*Handle) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultLockTimeout)
	defer cancel()
	return tx(ctx, path, true, fn)
}

// TxContext runs fn with a mutable TOML handle for path, using ctx while
// waiting to acquire the file lock.
func TxContext(ctx context.Context, path string, fn func(*Handle) error) error {
	return tx(ctx, path, true, fn)
}

// View runs fn with a read-only TOML handle for path.
//
// The handle type is still mutable for API simplicity, but View never writes
// callback changes back to disk.
func View(path string, fn func(*Handle) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultLockTimeout)
	defer cancel()
	return tx(ctx, path, false, fn)
}

// ViewContext runs fn with a read-only TOML handle for path, using ctx while
// waiting to acquire the file lock.
//
// The handle type is still mutable for API simplicity, but ViewContext never
// writes callback changes back to disk.
func ViewContext(ctx context.Context, path string, fn func(*Handle) error) error {
	return tx(ctx, path, false, fn)
}

func tx(ctx context.Context, path string, write bool, fn func(*Handle) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if path == "" {
		return fmt.Errorf("toml path is required")
	}
	if fn == nil {
		return fmt.Errorf("toml transaction function is required")
	}
	if write {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("mkdir toml dir: %w", err)
		}
	} else if _, err := os.Stat(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fn(&Handle{data: map[string]any{}})
		}
		return fmt.Errorf("stat toml dir: %w", err)
	}

	unlock, err := acquireLock(ctx, path, write)
	if err != nil {
		return err
	}
	defer unlock()

	h, err := loadHandleUnlocked(path)
	if err != nil {
		return err
	}
	if err := fn(h); err != nil {
		return err
	}
	if write && h.changed {
		return writeHandleUnlocked(path, h)
	}
	return nil
}

// Load decodes the TOML file at path into v. A missing file is treated as empty.
func Load(path string, v any) error {
	return View(path, func(h *Handle) error {
		return h.Decode(v)
	})
}

// LoadContext decodes the TOML file at path into v, using ctx while waiting to
// acquire the file lock. A missing file is treated as empty.
func LoadContext(ctx context.Context, path string, v any) error {
	return ViewContext(ctx, path, func(h *Handle) error {
		return h.Decode(v)
	})
}

// Save replaces the TOML file at path with v.
func Save(path string, v any) error {
	return Tx(path, func(h *Handle) error {
		h.Replace(v)
		return nil
	})
}

// SaveContext replaces the TOML file at path with v, using ctx while waiting to
// acquire the file lock.
func SaveContext(ctx context.Context, path string, v any) error {
	return TxContext(ctx, path, func(h *Handle) error {
		h.Replace(v)
		return nil
	})
}

// Delete removes the TOML file at path. A missing file is not an error.
func Delete(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultLockTimeout)
	defer cancel()
	return DeleteContext(ctx, path)
}

// DeleteContext removes the TOML file at path, using ctx while waiting to
// acquire the file lock. A missing file is not an error.
func DeleteContext(ctx context.Context, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if path == "" {
		return fmt.Errorf("toml path is required")
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat toml dir: %w", err)
	}
	unlock, err := acquireLock(ctx, path, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete toml file: %w", err)
	}
	return nil
}

// Get returns the value at dotted path.
func (h *Handle) Get(path string) (any, bool) {
	if h == nil {
		return nil, false
	}
	parts, err := splitPath(path)
	if err != nil {
		return nil, false
	}
	m := h.data
	for i, part := range parts {
		v, ok := m[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return v, true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	return nil, false
}

// Set assigns value at dotted path, creating intermediate tables as needed.
func (h *Handle) Set(path string, value any) error {
	if h == nil {
		return fmt.Errorf("nil toml handle")
	}
	parts, err := splitPath(path)
	if err != nil {
		return err
	}
	m := h.data
	for _, part := range parts[:len(parts)-1] {
		next, ok := m[part].(map[string]any)
		if !ok {
			if _, exists := m[part]; exists {
				return fmt.Errorf("toml path %q crosses non-table key %q", path, part)
			}
			next = map[string]any{}
			m[part] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = normalizeValue(value)
	h.changed = true
	return nil
}

// Delete removes the value at dotted path. Missing paths are ignored.
func (h *Handle) Delete(path string) error {
	if h == nil {
		return fmt.Errorf("nil toml handle")
	}
	parts, err := splitPath(path)
	if err != nil {
		return err
	}
	m := h.data
	for _, part := range parts[:len(parts)-1] {
		next, ok := m[part].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	if _, ok := m[parts[len(parts)-1]]; ok {
		delete(m, parts[len(parts)-1])
		h.changed = true
	}
	return nil
}

// String returns the string value at dotted path.
func (h *Handle) String(path string) string {
	v, ok := h.Get(path)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// StringSlice returns the string slice value at dotted path.
func (h *Handle) StringSlice(path string) []string {
	v, ok := h.Get(path)
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, elem := range x {
			s, ok := elem.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

// Bool returns the bool value at dotted path.
func (h *Handle) Bool(path string) bool {
	v, ok := h.Get(path)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// Decode decodes the whole TOML handle into v.
func (h *Handle) Decode(v any) error {
	b, err := toml.Marshal(h.data)
	if err != nil {
		return fmt.Errorf("marshal toml handle: %w", err)
	}
	if err := toml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode toml handle: %w", err)
	}
	return nil
}

// Replace replaces the whole TOML handle with v.
func (h *Handle) Replace(v any) error {
	b, err := toml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal toml replacement: %w", err)
	}
	data, err := parseTOMLMap(b)
	if err != nil {
		return err
	}
	h.data = data
	h.changed = true
	return nil
}

func loadHandleUnlocked(path string) (*Handle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Handle{data: map[string]any{}}, nil
		}
		return nil, fmt.Errorf("read toml file: %w", err)
	}
	data, err := parseTOMLMap(b)
	if err != nil {
		return nil, err
	}
	return &Handle{data: data}, nil
}

func writeHandleUnlocked(path string, h *Handle) error {
	b, err := toml.Marshal(h.data)
	if err != nil {
		return fmt.Errorf("marshal toml file: %w", err)
	}
	// The file is replaced atomically below. Remember its metadata so that the
	// replacement does not acquire the identity and umask-derived mode of the
	// process doing the write (in particular, when Weaver is run via sudo).
	var existing os.FileInfo
	if info, statErr := os.Stat(path); statErr == nil {
		existing = info
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat toml file: %w", statErr)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create toml temp file: %w", err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if existing != nil {
		if err := preserveFileOwner(tmp, existing); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("preserve toml file owner: %w", err)
		}
	}
	mode := os.FileMode(0o600)
	if existing != nil {
		mode = existing.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod toml temp file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write toml temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close toml temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace toml file: %w", err)
	}
	removeTmp = false
	return nil
}

func parseTOMLMap(b []byte) (map[string]any, error) {
	var data map[string]any
	if len(b) > 0 {
		if err := toml.Unmarshal(b, &data); err != nil {
			return nil, fmt.Errorf("parse toml file: %w", err)
		}
	}
	if data == nil {
		data = map[string]any{}
	}
	return normalizeMap(data), nil
}

func splitPath(path string) ([]string, error) {
	parts := strings.Split(path, ".")
	out := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("invalid toml path %q", path)
		}
		out = append(out, part)
	}
	return out, nil
}

func normalizeMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return normalizeMap(x)
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[fmt.Sprint(k)] = normalizeValue(v)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = normalizeValue(elem)
		}
		return out
	default:
		return v
	}
}

func acquireLock(ctx context.Context, path string, exclusive bool) (func() error, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open toml lock: %w", err)
	}
	if err := lockFile(ctx, f, exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		unlockErr := unlockFile(f)
		closeErr := f.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}
