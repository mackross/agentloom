package tomlfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTxLoadsFromDiskBeforeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[brave]\napi_key = 'brave-1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	staleAccessToken := "stale-access"
	if err := os.WriteFile(path, []byte("[brave]\napi_key = 'brave-2'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Tx(path, func(h *Handle) error {
		if err := h.Set("openai.codex.access_token", staleAccessToken); err != nil {
			return err
		}
		return h.Set("openai.codex.access_token", "fresh-access")
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `api_key = 'brave-2'`) {
		t.Fatalf("did not preserve latest on-disk brave section:\n%s", s)
	}
	if !strings.Contains(s, `access_token = 'fresh-access'`) {
		t.Fatalf("did not write updated openai section:\n%s", s)
	}
}

func TestTxPreservesExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.toml")
	if err := os.WriteFile(path, []byte("[brave]\napi_key = 'old'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Tx(path, func(h *Handle) error {
		return h.Set("brave.api_key", "new")
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %04o, want 0640", got)
	}
}

func TestTxDottedPathAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Tx(path, func(h *Handle) error {
		if err := h.Set("openai.codex.access_token", "access-1"); err != nil {
			return err
		}
		if err := h.Set("openai.codex.scope", []string{"api.responses.write"}); err != nil {
			return err
		}
		return h.Set("openai.codex.prefer_codex_oauth", true)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if err := View(path, func(h *Handle) error {
		if got := h.String("openai.codex.access_token"); got != "access-1" {
			t.Fatalf("access token = %q", got)
		}
		if got := h.StringSlice("openai.codex.scope"); len(got) != 1 || got[0] != "api.responses.write" {
			t.Fatalf("scope = %#v", got)
		}
		if !h.Bool("openai.codex.prefer_codex_oauth") {
			t.Fatalf("prefer_codex_oauth not true")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestConcurrentTxPreserveBothPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	start := make(chan struct{})
	var wg sync.WaitGroup
	var errs [2]error

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = Tx(path, func(h *Handle) error {
			return h.Set("brave.api_key", "brave-1")
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = Tx(path, func(h *Handle) error {
			return h.Set("openai.codex.access_token", "access-1")
		})
	}()
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Tx: %v", err)
		}
	}

	if err := View(path, func(h *Handle) error {
		if got := h.String("brave.api_key"); got != "brave-1" {
			t.Fatalf("brave.api_key = %q", got)
		}
		if got := h.String("openai.codex.access_token"); got != "access-1" {
			t.Fatalf("openai.codex.access_token = %q", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestTxContextTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Tx(path, func(h *Handle) error {
			close(locked)
			<-release
			return h.Set("holder.locked", true)
		})
	}()
	<-locked

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	err := TxContext(ctx, path, func(h *Handle) error {
		t.Fatalf("callback should not run after lock timeout")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TxContext err = %v, want context deadline exceeded", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder Tx: %v", err)
	}
}

func TestDeleteMissingOK(t *testing.T) {
	if err := Delete(filepath.Join(t.TempDir(), "missing", "config.toml")); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}
