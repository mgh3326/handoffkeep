// Package cfaccess verifies Cloudflare Access assertions for the optional UI.
package cfaccess

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const assertionHeader = "Cf-Access-Jwt-Assertion"

// Config is deliberately small enough to make a verifier testable without a
// network dependency. Issuer and CertsURL are test-only overrides; production
// environment wiring leaves both unset and derives them from TeamDomain.
type Config struct {
	TeamDomain    string
	AUD           string
	AllowedEmails []string
	Issuer        string
	CertsURL      string
	HTTPClient    *http.Client
	CacheTTL      time.Duration
}

// Verifier owns the short-lived JWKS cache used by one UI handler.
type Verifier struct {
	issuer  string
	aud     string
	certs   string
	client  *http.Client
	ttl     time.Duration
	allowed map[string]struct{}

	mu                    sync.Mutex
	keys                  map[string]*rsa.PublicKey
	fetchedAt             time.Time
	lastUnknownKidRefresh time.Time
	refreshMu             sync.Mutex
}

// New constructs a fail-closed verifier.
func New(config Config) (*Verifier, error) {
	team := strings.TrimSpace(config.TeamDomain)
	aud := strings.TrimSpace(config.AUD)
	if team == "" || aud == "" {
		return nil, errors.New("Cloudflare Access configuration is incomplete")
	}
	allowed := make(map[string]struct{}, len(config.AllowedEmails))
	for _, email := range config.AllowedEmails {
		email = asciiLower(strings.TrimSpace(email))
		if email != "" {
			allowed[email] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, errors.New("Cloudflare Access email allowlist is empty")
	}
	issuer := strings.TrimSpace(config.Issuer)
	if issuer == "" {
		issuer = "https://" + team
	}
	certs := strings.TrimSpace(config.CertsURL)
	if certs == "" {
		certs = "https://" + team + "/cdn-cgi/access/certs"
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	ttl := config.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Verifier{issuer: issuer, aud: aud, certs: certs, client: client, ttl: ttl, allowed: allowed}, nil
}

// Authenticate verifies the assertion in r. It intentionally returns no
// diagnostic detail so callers can always produce a uniform 401 response.
func (v *Verifier) Authenticate(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get(assertionHeader))
	if raw == "" {
		return false
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || strings.TrimSpace(kid) == "" {
			return nil, errors.New("missing signing key")
		}
		return v.keyFor(r.Context(), kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience(v.aud), jwt.WithIssuer(v.issuer), jwt.WithExpirationRequired())
	if err != nil {
		return false
	}
	email, ok := claims["email"].(string)
	if !ok {
		return false
	}
	_, ok = v.allowed[asciiLower(strings.TrimSpace(email))]
	return ok
}

func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now := time.Now()
	v.mu.Lock()
	key := v.keys[kid]
	fresh := v.keys != nil && now.Sub(v.fetchedAt) < v.ttl
	refreshUnknown := key == nil && (v.lastUnknownKidRefresh.IsZero() || now.Sub(v.lastUnknownKidRefresh) >= time.Minute)
	if refreshUnknown {
		v.lastUnknownKidRefresh = now
	}
	v.mu.Unlock()
	if key != nil && fresh {
		return key, nil
	}
	if key == nil && !refreshUnknown {
		return nil, errors.New("unknown signing key")
	}
	if err := v.refresh(ctx); err != nil {
		// A fresh cache can still validate a known key when an ordinary refresh
		// fails. Missing keys never use this fallback.
		v.mu.Lock()
		cached, cacheFresh := v.keys[kid], v.keys != nil && time.Since(v.fetchedAt) < v.ttl
		v.mu.Unlock()
		if cached != nil && cacheFresh {
			return cached, nil
		}
		return nil, err
	}
	v.mu.Lock()
	key = v.keys[kid]
	v.mu.Unlock()
	if key == nil {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (v *Verifier) refresh(ctx context.Context) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certs, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("JWKS request failed")
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("JWKS response was not JSON")
	}
	var document struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, raw := range document.Keys {
		key, ok := raw.rsaKey()
		if ok {
			keys[raw.Kid] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no RSA keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j jwk) rsaKey() (*rsa.PublicKey, bool) {
	if j.Kty != "RSA" || j.Kid == "" || j.N == "" || j.E == "" {
		return nil, false
	}
	n, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil || len(n) == 0 {
		return nil, false
	}
	e, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil || len(e) == 0 || len(e) > 8 {
		return nil, false
	}
	exponent := 0
	for _, b := range e {
		exponent = exponent<<8 | int(b)
	}
	if exponent < 2 {
		return nil, false
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}, true
}

func asciiLower(value string) string {
	b := []byte(value)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
