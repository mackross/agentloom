// Package toolcallcache stores oversized tool outputs for later reads.
package toolcallcache

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

const dirName = "weaver"

var alphabet = []byte("abcdefghijklmnopqrstuvwxyz0123456789")

// Entry identifies cached tool output.
type Entry struct {
	ID   string
	Path string
}

// Put writes content to a temp-dir cache file and returns its entry.
//
// Paths are intentionally short for model readability:
//
//	$TMPDIR/weaver/<2 chars>/<6 chars>
func Put(_ string, content string) (Entry, error) {
	shard, name, err := newPathParts()
	if err != nil {
		return Entry{}, err
	}
	dir := filepath.Join(os.TempDir(), dirName, shard)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Entry{}, err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return Entry{}, err
	}
	return Entry{ID: shard + "/" + name, Path: path}, nil
}

func newPathParts() (string, string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generate cache id: %w", err)
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out[:2]), string(out[2:]), nil
}
