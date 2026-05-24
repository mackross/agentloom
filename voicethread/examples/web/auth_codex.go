package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	defaultOpenAITokenURL = "https://auth.openai.com/oauth/token"
	defaultOpenAIClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var defaultOpenAIOAuthScopes = []string{"api.responses.write"}

type codexAuthConfig struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       string
	Scope        []string
	ClientID     string
	ClientSecret string
	TokenURL     string
}

func openAIAPIKeyFromEnvOrCodexAuth(ctx context.Context) (string, string, error) {
	if apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); apiKey != "" && !envBool("VOICE_USE_CODEX_AUTH") {
		return apiKey, "OPENAI_API_KEY", nil
	}
	path, err := codexAuthPath()
	if err != nil {
		return "", "", err
	}
	sec, err := readCodexAuth(path)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(sec.AccessToken) == "" && strings.TrimSpace(sec.RefreshToken) == "" {
		return "", "", fmt.Errorf("set OPENAI_API_KEY or run weaver codex auth to populate %s", path)
	}
	tok, err := codexAuthToken(ctx, sec)
	if err != nil {
		return "", "", err
	}
	if tok.AccessToken == "" {
		return "", "", fmt.Errorf("codex auth at %s produced no access token", path)
	}
	return tok.AccessToken, path, nil
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func codexAuthToken(ctx context.Context, sec codexAuthConfig) (*oauth2.Token, error) {
	tok := &oauth2.Token{
		AccessToken:  strings.TrimSpace(sec.AccessToken),
		RefreshToken: strings.TrimSpace(sec.RefreshToken),
		TokenType:    strings.TrimSpace(sec.TokenType),
	}
	if tok.TokenType == "" {
		tok.TokenType = "Bearer"
	}
	if expiry := strings.TrimSpace(sec.Expiry); expiry != "" {
		t, err := time.Parse(time.RFC3339, expiry)
		if err != nil {
			return nil, fmt.Errorf("invalid codex auth expires_at: %w", err)
		}
		tok.Expiry = t
	}
	if strings.TrimSpace(sec.RefreshToken) == "" {
		return tok, nil
	}
	clientID := strings.TrimSpace(sec.ClientID)
	if clientID == "" {
		clientID = defaultOpenAIClientID
	}
	clientSecret := strings.TrimSpace(sec.ClientSecret)
	if clientSecret == "" {
		clientSecret = strings.TrimSpace(os.Getenv("OPENAI_OAUTH_CLIENT_SECRET"))
	}
	tokenURL := strings.TrimSpace(sec.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultOpenAITokenURL
	}
	scopes := sec.Scope
	if len(scopes) == 0 {
		scopes = defaultOpenAIOAuthScopes
	}
	return (&oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       scopes,
		Endpoint:     oauth2.Endpoint{TokenURL: tokenURL},
	}).TokenSource(ctx, tok).Token()
}

func codexAuthPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(configDir, "weaver", "auth.toml"), nil
}

func readCodexAuth(path string) (codexAuthConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return codexAuthConfig{}, nil
		}
		return codexAuthConfig{}, err
	}
	defer f.Close()
	var sec codexAuthConfig
	inCodex := false
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inCodex = strings.TrimSpace(strings.Trim(line, "[]")) == "openai.codex"
			continue
		}
		if !inCodex {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(strings.SplitN(val, "#", 2)[0])
		switch key {
		case "access_token":
			sec.AccessToken = tomlString(val)
		case "refresh_token":
			sec.RefreshToken = tomlString(val)
		case "token_type":
			sec.TokenType = tomlString(val)
		case "expires_at":
			sec.Expiry = tomlString(val)
		case "scope":
			sec.Scope = tomlStringSlice(val)
		case "client_id":
			sec.ClientID = tomlString(val)
		case "client_secret":
			sec.ClientSecret = tomlString(val)
		case "token_url":
			sec.TokenURL = tomlString(val)
		}
	}
	if err := s.Err(); err != nil {
		return codexAuthConfig{}, err
	}
	return sec, nil
}

func tomlString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		q := v[0]
		if (q == '\'' || q == '"') && v[len(v)-1] == q {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func tomlStringSlice(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimSuffix(v, "]"), "[")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := strings.TrimSpace(tomlString(part)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
