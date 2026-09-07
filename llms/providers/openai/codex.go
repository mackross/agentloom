package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	openaiapi "github.com/mackross/openai-go/v3"
	"github.com/mackross/openai-go/v3/option"
	"golang.org/x/oauth2"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/subscription"
	"github.com/mackross/agentloom/threads"
)

// Codex endpoints. Package-level so tests can point them at a local server.
var (
	codexIssuer  = "https://auth.openai.com"
	codexBackend = "https://chatgpt.com/backend-api/codex"
)

const (
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexScope        = "api.responses.write"
	codexPollDeadline = 15 * time.Minute
)

// Codex signs in to a ChatGPT subscription with OpenAI's device
// authorization flow so the Responses API can be used through the Codex
// backend.
type Codex struct {
	// HTTPClient is used for the sign-in requests. nil uses http.DefaultClient.
	HTTPClient *http.Client
}

func (Codex) Name() string { return "OpenAI Codex" }

func (c Codex) SignIn(ctx context.Context, show func(llms.SignInPrompt)) (*llms.Token, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	deviceURL := codexIssuer + "/api/accounts/deviceauth"

	var device struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Usercode     string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if _, err := postJSON(ctx, hc, deviceURL+"/usercode", map[string]string{"client_id": codexClientID}, &device); err != nil {
		return nil, fmt.Errorf("codex device code: %w", err)
	}
	userCode := device.UserCode
	if userCode == "" {
		userCode = device.Usercode
	}
	if device.DeviceAuthID == "" || userCode == "" {
		return nil, errors.New("codex device code: response missing device_auth_id or user_code")
	}
	interval := parseInterval(device.Interval)
	if show != nil {
		show(llms.SignInPrompt{URL: codexIssuer + "/codex/device", Code: userCode, Interval: interval})
	}

	var grant struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	deadline := time.Now().Add(codexPollDeadline)
	for {
		status, err := postJSON(ctx, hc, deviceURL+"/token", map[string]string{"device_auth_id": device.DeviceAuthID, "user_code": userCode}, &grant)
		if err == nil {
			break
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return nil, fmt.Errorf("codex device poll: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, errors.New("codex device poll: timed out waiting for authorization")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
	if grant.AuthorizationCode == "" {
		return nil, errors.New("codex device poll: response missing authorization_code")
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	tok, err := codexOAuth().Exchange(ctx, grant.AuthorizationCode,
		oauth2.VerifierOption(grant.CodeVerifier),
		oauth2.SetAuthURLParam("scope", codexScope))
	if err != nil {
		return nil, fmt.Errorf("codex token exchange: %w", err)
	}
	return subscription.FromOAuth2(tok), nil
}

func codexOAuth() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     codexClientID,
		ClientSecret: strings.TrimSpace(os.Getenv("OPENAI_OAUTH_CLIENT_SECRET")),
		Scopes:       []string{codexScope},
		RedirectURL:  codexIssuer + "/deviceauth/callback",
		Endpoint:     oauth2.Endpoint{TokenURL: codexIssuer + "/oauth/token", AuthStyle: oauth2.AuthStyleInParams},
	}
}

func openCodex(ctx context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
	src := subscription.TokenSource(ctx, codexOAuth(), o.Token, o.OnRefresh, o.HTTPClient)
	mw := func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if err := subscription.Authorize(src, req, func(tok *oauth2.Token, h http.Header) {
			if id := chatGPTAccountID(tok.AccessToken); id != "" {
				h.Set("ChatGPT-Account-ID", id)
			}
		}); err != nil {
			return nil, err
		}
		return next(req)
	}
	base := o.BaseURL
	if base == "" {
		base = codexBackend
	}
	opts := []option.RequestOption{option.WithAPIKey("oauth"), option.WithBaseURL(base), option.WithMiddleware(mw)}
	if o.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(o.HTTPClient))
	}
	return NewResponsesStreamerWithClient(openaiapi.NewClient(opts...), m.ID), nil
}

// chatGPTAccountID extracts the ChatGPT account id claim from a Codex access
// token. It returns "" when the token is not a JWT carrying the claim.
func chatGPTAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	id, _ := auth["chatgpt_account_id"].(string)
	return id
}

func postJSON(ctx context.Context, hc *http.Client, url string, body any, out any) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func parseInterval(raw json.RawMessage) time.Duration {
	const fallback = 5 * time.Second
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return fallback
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Second))
	}
	return fallback
}
