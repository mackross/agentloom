package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mackross/agentloom/llms"
)

func TestDeviceFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device/code":
			if r.Form.Get("client_id") != "cid" {
				t.Errorf("client_id = %q", r.Form.Get("client_id"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "USER-1", "verification_uri": "https://x/verify", "interval": 0, "expires_in": 60})
		case "/token":
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dc" {
				t.Errorf("token form = %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a1", "refresh_token": "r1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "cid", Endpoint: oauth2.Endpoint{TokenURL: srv.URL + "/token", DeviceAuthURL: srv.URL + "/device/code", AuthStyle: oauth2.AuthStyleInParams}}
	var prompt llms.SignInPrompt
	tok, err := DeviceFlow("test", cfg).SignIn(context.Background(), func(p llms.SignInPrompt) { prompt = p })
	if err != nil {
		t.Fatal(err)
	}
	if prompt.URL != "https://x/verify" || prompt.Code != "USER-1" {
		t.Fatalf("prompt %+v", prompt)
	}
	if tok.Access != "a1" || tok.Refresh != "r1" {
		t.Fatalf("token %+v", tok)
	}
}

func TestTokenSourceRefreshPersistsOnce(t *testing.T) {
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" {
			t.Errorf("form = %v", r.Form)
		}
		refreshes++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a2", "refresh_token": "r2", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "cid", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	var saved []*llms.Token
	expired := &llms.Token{Access: "a1", Refresh: "r1", Expiry: time.Now().Add(-time.Hour)}
	src := TokenSource(context.Background(), cfg, expired, func(t *llms.Token) error { saved = append(saved, t); return nil }, nil)
	for i := 0; i < 3; i++ {
		tok, err := src.Token()
		if err != nil || tok.AccessToken != "a2" {
			t.Fatalf("Token() = %+v, %v", tok, err)
		}
	}
	if refreshes != 1 || len(saved) != 1 || saved[0].Refresh != "r2" {
		t.Fatalf("refreshes=%d saved=%+v", refreshes, saved)
	}
	static := TokenSource(context.Background(), cfg, &llms.Token{Access: "only"}, nil, nil)
	if tok, _ := static.Token(); tok.AccessToken != "only" {
		t.Fatal("static source")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://h", nil)
	if err := Authorize(static, req, func(_ *oauth2.Token, h http.Header) { h.Set("X-Extra", "1") }); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer only" || req.Header.Get("X-Extra") != "1" {
		t.Fatalf("headers %v", req.Header)
	}
}
