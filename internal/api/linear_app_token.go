package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	linearTokenURL = "https://api.linear.app/oauth/token"
	// linearTokenRenew renews an app token this long before it expires.
	linearTokenRenew = 24 * time.Hour
)

// LinearAppToken mints actor=app access tokens for a Linear OAuth application
// with the client_credentials grant and renews them before they expire. Writes
// made with it show as the application, never as a workspace member, so an
// agent using hk can not pass for the operator on the webhook.
type LinearAppToken struct {
	ClientID     string
	ClientSecret string
	URL          string // empty means Linear's production token endpoint
	HTTP         *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a token valid for at least linearTokenRenew, minting one when
// none is cached or the cached one is close to expiry.
func (t *LinearAppToken) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}
	if t.token != "" && now.Add(linearTokenRenew).Before(t.expires) {
		return t.token, nil
	}
	endpoint := t.URL
	if endpoint == "" {
		endpoint = linearTokenURL
	}
	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {t.ClientID}, "client_secret": {t.ClientSecret}, "scope": {"read,write"}}
	ctx, cancel := context.WithTimeout(ctx, linearAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body can echo request fields; only the status is reported.
		return "", fmt.Errorf("linear token status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("linear token response: %w", err)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("linear token response has no token or expiry")
	}
	t.token = out.AccessToken
	t.expires = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return t.token, nil
}

// Invalidate drops the cached token so the next call mints a new one.
func (t *LinearAppToken) Invalidate() {
	t.mu.Lock()
	t.token = ""
	t.mu.Unlock()
}
