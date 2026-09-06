// Package subscription holds the provider-agnostic parts of subscription
// sign-in: the RFC 8628 device flow and refreshing token sources that report
// new tokens back to the caller for persistence.
package subscription

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/mackross/agentloom/llms"
)

// DeviceFlow returns a Subscription that signs in with the OAuth device
// authorization grant (RFC 8628) against cfg.
func DeviceFlow(name string, cfg *oauth2.Config) llms.Subscription {
	return deviceFlow{name: name, cfg: cfg}
}

type deviceFlow struct {
	name string
	cfg  *oauth2.Config
}

func (d deviceFlow) Name() string { return d.name }

func (d deviceFlow) SignIn(ctx context.Context, show func(llms.SignInPrompt)) (*llms.Token, error) {
	resp, err := d.cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s device authorization: %w", d.name, err)
	}
	if show != nil {
		show(llms.SignInPrompt{
			URL:      resp.VerificationURI,
			Code:     resp.UserCode,
			Interval: time.Duration(resp.Interval) * time.Second,
		})
	}
	tok, err := d.cfg.DeviceAccessToken(ctx, resp)
	if err != nil {
		return nil, fmt.Errorf("%s device token: %w", d.name, err)
	}
	return FromOAuth2(tok), nil
}

// TokenSource returns a token source for tok. Tokens without a refresh token
// are static. Otherwise the source refreshes through cfg and calls onRefresh
// once per new token so the caller can persist it. The returned source does
// not hold on to ctx's cancellation; only its values (such as
// oauth2.HTTPClient) are kept.
func TokenSource(ctx context.Context, cfg *oauth2.Config, tok *llms.Token, onRefresh func(*llms.Token) error, hc *http.Client) oauth2.TokenSource {
	ot := ToOAuth2(tok)
	if cfg == nil || ot.RefreshToken == "" {
		return oauth2.StaticTokenSource(ot)
	}
	ctx = context.WithoutCancel(ctx)
	if hc != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	}
	return &persisting{src: cfg.TokenSource(ctx, ot), last: fingerprint(ot), save: onRefresh}
}

type persisting struct {
	mu   sync.Mutex
	src  oauth2.TokenSource
	last string
	save func(*llms.Token) error
}

func (p *persisting) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fp := fingerprint(tok); fp != p.last {
		p.last = fp
		if p.save != nil {
			if err := p.save(FromOAuth2(tok)); err != nil {
				return nil, fmt.Errorf("persist refreshed token: %w", err)
			}
		}
	}
	return tok, nil
}

// Authorize sets the bearer token from src on req. extra, if non-nil, may add
// further headers derived from the token.
func Authorize(src oauth2.TokenSource, req *http.Request, extra func(*oauth2.Token, http.Header)) error {
	tok, err := src.Token()
	if err != nil {
		return fmt.Errorf("subscription token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if extra != nil {
		extra(tok, req.Header)
	}
	return nil
}

// FromOAuth2 converts an oauth2 token.
func FromOAuth2(t *oauth2.Token) *llms.Token {
	if t == nil {
		return nil
	}
	return &llms.Token{Access: t.AccessToken, Refresh: t.RefreshToken, Expiry: t.Expiry}
}

// ToOAuth2 converts to an oauth2 token.
func ToOAuth2(t *llms.Token) *oauth2.Token {
	if t == nil {
		return &oauth2.Token{}
	}
	return &oauth2.Token{AccessToken: t.Access, RefreshToken: t.Refresh, Expiry: t.Expiry, TokenType: "Bearer"}
}

func fingerprint(t *oauth2.Token) string {
	return t.AccessToken + "|" + t.RefreshToken + "|" + t.Expiry.UTC().Format(time.RFC3339Nano)
}
