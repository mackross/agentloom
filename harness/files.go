package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mackross/agentloom/harness/internal/tomlfile"
	"github.com/mackross/agentloom/llms"
)

// DefaultDir returns the shared configuration directory: $AGENTS_CONFIG_DIR,
// else $XDG_CONFIG_HOME/agents, else ~/.config/agents (the platform config
// directory plus "agents" on Windows).
func DefaultDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("AGENTS_CONFIG_DIR")); dir != "" {
		return dir, nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "agents"), nil
	}
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("harness: config dir: %w", err)
		}
		return filepath.Join(base, "agents"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("harness: config dir: %w", err)
	}
	return filepath.Join(home, ".config", "agents"), nil
}

// Files returns a Store over TOML files in dir. The files carry a ".loom.toml"
// suffix so the directory can be shared with other tools: models.loom.toml
// holds shared settings and model overlays, models.<app>.loom.toml overrides
// it key by key and receives writes, and auth.loom.toml holds credentials.
// With an empty app, writes go to models.loom.toml.
func Files(dir, app string) Store {
	f := &files{
		models: filepath.Join(dir, "models.loom.toml"),
		auth:   filepath.Join(dir, "auth.loom.toml"),
	}
	if app = strings.TrimSpace(app); app != "" {
		f.app = filepath.Join(dir, "models."+app+".loom.toml")
	}
	return f
}

type files struct {
	models, app, auth string
}

func (f *files) target() string {
	if f.app != "" {
		return f.app
	}
	return f.models
}

func (f *files) Load(ctx context.Context) (Settings, error) {
	s := Settings{Providers: map[string]ProviderSettings{}, Auth: map[string]AuthEntry{}}
	for _, path := range []string{f.models, f.app} {
		if path == "" {
			continue
		}
		if err := readModels(ctx, path, &s); err != nil {
			return Settings{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := readAuth(ctx, f.auth, &s); err != nil {
		return Settings{}, fmt.Errorf("%s: %w", f.auth, err)
	}
	return s, nil
}

func (f *files) Save(ctx context.Context, fn func(*Settings) error) error {
	before, err := f.Load(ctx)
	if err != nil {
		return err
	}
	after := before.clone()
	if err := fn(&after); err != nil {
		return err
	}
	err = tomlfile.TxContext(ctx, f.target(), func(h *tomlfile.Handle) error {
		if after.Default != before.Default {
			if err := setOrDelete(h, "default", after.Default, after.Default == ""); err != nil {
				return err
			}
		}
		if after.Effort != before.Effort {
			if err := setOrDelete(h, "effort", string(after.Effort), after.Effort == llms.EffortDefault); err != nil {
				return err
			}
		}
		if after.Fast != before.Fast {
			if err := h.Set("fast", after.Fast); err != nil {
				return err
			}
		}
		for id, ps := range after.Providers {
			if ps.Model != before.provider(id).Model {
				if err := setOrDelete(h, "providers."+id+".model", ps.Model, ps.Model == ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", f.target(), err)
	}
	err = tomlfile.TxContext(ctx, f.auth, func(h *tomlfile.Handle) error {
		ids := map[string]bool{}
		for id := range before.Auth {
			ids[id] = true
		}
		for id := range after.Auth {
			ids[id] = true
		}
		for id := range ids {
			a, b := after.auth(id), before.auth(id)
			if a.APIKey != b.APIKey {
				if err := setOrDelete(h, id+".api_key", a.APIKey, a.APIKey == ""); err != nil {
					return err
				}
			}
			if !sameToken(a.Token, b.Token) {
				if a.Token == nil {
					if err := h.Delete(id + ".subscription"); err != nil {
						return err
					}
					continue
				}
				for k, v := range map[string]any{
					"access_token":  a.Token.Access,
					"refresh_token": a.Token.Refresh,
					"expires_at":    a.Token.Expiry.UTC(),
				} {
					if err := h.Set(id+".subscription."+k, v); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", f.auth, err)
	}
	return nil
}

func setOrDelete(h *tomlfile.Handle, path string, v any, del bool) error {
	if del {
		return h.Delete(path)
	}
	return h.Set(path, v)
}

func sameToken(a, b *llms.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Access == b.Access && a.Refresh == b.Refresh && a.Expiry.Equal(b.Expiry)
}

func readModels(ctx context.Context, path string, s *Settings) error {
	return tomlfile.ViewContext(ctx, path, func(h *tomlfile.Handle) error {
		if v, ok := h.Get("default"); ok {
			s.Default, _ = v.(string)
		}
		if v, ok := h.Get("effort"); ok {
			raw, _ := v.(string)
			e, err := llms.ParseEffort(raw)
			if err != nil {
				return fmt.Errorf("effort: %w", err)
			}
			s.Effort = e
		}
		if v, ok := h.Get("fast"); ok {
			s.Fast, _ = v.(bool)
		}
		if providers, ok := h.Get("providers"); ok {
			table, _ := providers.(map[string]any)
			for id, raw := range table {
				entry, _ := raw.(map[string]any)
				if model, ok := entry["model"].(string); ok {
					s.setProviderModel(id, strings.TrimSpace(model))
				}
			}
		}
		if models, ok := h.Get("models"); ok {
			list, _ := models.([]any)
			for i, raw := range list {
				table, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("models[%d]: not a table", i)
				}
				m, err := decodeModel(table)
				if err != nil {
					return fmt.Errorf("models[%d]: %w", i, err)
				}
				s.Models = append(s.Models, m)
			}
		}
		return nil
	})
}

func decodeModel(table map[string]any) (llms.Model, error) {
	var m llms.Model
	for key, v := range table {
		switch key {
		case "provider":
			m.Provider, _ = v.(string)
		case "name":
			m.Name, _ = v.(string)
		case "id":
			m.ID, _ = v.(string)
		case "label":
			m.Label, _ = v.(string)
		case "aliases":
			m.Aliases = strings2(v)
		case "fast":
			m.Fast, _ = v.(bool)
		case "efforts":
			for _, raw := range strings2(v) {
				e, err := llms.ParseEffort(raw)
				if err != nil || e == llms.EffortDefault {
					return m, fmt.Errorf("efforts: invalid level %q", raw)
				}
				m.Efforts = append(m.Efforts, e)
			}
		default:
			if m.Options == nil {
				m.Options = map[string]any{}
			}
			m.Options[key] = v
		}
	}
	if m.Provider == "" {
		return m, fmt.Errorf("missing provider")
	}
	if m.Name == "" && m.ID == "" {
		return m, fmt.Errorf("missing name")
	}
	return m, nil
}

func strings2(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func readAuth(ctx context.Context, path string, s *Settings) error {
	return tomlfile.ViewContext(ctx, path, func(h *tomlfile.Handle) error {
		var raw map[string]any
		if err := h.Decode(&raw); err != nil {
			return err
		}
		for id, v := range raw {
			table, ok := v.(map[string]any)
			if !ok {
				continue
			}
			s.setAuth(id, func(e *AuthEntry) {
				e.APIKey, _ = table["api_key"].(string)
				if sub, ok := table["subscription"].(map[string]any); ok {
					tok := &llms.Token{}
					tok.Access, _ = sub["access_token"].(string)
					tok.Refresh, _ = sub["refresh_token"].(string)
					switch t := sub["expires_at"].(type) {
					case time.Time:
						tok.Expiry = t
					case string:
						tok.Expiry, _ = time.Parse(time.RFC3339, t)
					}
					if tok.Access != "" || tok.Refresh != "" {
						e.Token = tok
					}
				}
			})
		}
		return nil
	})
}
