package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
)

func TestCodexSignIn(t *testing.T) {
	polls := 0
	var exchange map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != codexClientID {
				t.Errorf("client_id = %q", body["client_id"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "dev-1", "user_code": "ABCD-EFGH", "interval": "0"})
		case "/api/accounts/deviceauth/token":
			polls++
			if polls < 2 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_code": "code-1", "code_verifier": "verifier-1"})
		case "/oauth/token":
			_ = r.ParseForm()
			exchange = r.Form
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := codexIssuer
	codexIssuer = srv.URL
	defer func() { codexIssuer = old }()

	var prompt llms.SignInPrompt
	tok, err := Codex{}.SignIn(context.Background(), func(p llms.SignInPrompt) { prompt = p })
	if err != nil {
		t.Fatal(err)
	}
	if prompt.URL != srv.URL+"/codex/device" || prompt.Code != "ABCD-EFGH" {
		t.Fatalf("prompt: %+v", prompt)
	}
	if tok.Access != "access-1" || tok.Refresh != "refresh-1" || tok.Expiry.IsZero() {
		t.Fatalf("token: %+v", tok)
	}
	if polls != 2 {
		t.Fatalf("polls = %d", polls)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     codexClientID,
		"code":          "code-1",
		"code_verifier": "verifier-1",
		"redirect_uri":  srv.URL + "/deviceauth/callback",
		"scope":         codexScope,
	}
	for k, v := range want {
		if got := strings.Join(exchange[k], ","); got != v {
			t.Errorf("exchange %s = %q, want %q", k, got, v)
		}
	}
}

func TestChatGPTAccountID(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-42"}})
	jwt := "hdr." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	if got := chatGPTAccountID(jwt); got != "acct-42" {
		t.Fatalf("got %q", got)
	}
	if chatGPTAccountID("not-a-jwt") != "" {
		t.Fatal("expected empty for opaque token")
	}
}
