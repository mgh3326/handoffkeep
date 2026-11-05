package cfaccess

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		n := base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "known", "kty": "RSA", "n": n, "e": e}}})
	}))
	defer certs.Close()
	verifier, err := New(Config{TeamDomain: "example.cloudflareaccess.com", AUD: "ui-audience", AllowedEmails: []string{"admin@example.com"}, Issuer: certs.URL, CertsURL: certs.URL})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(assertionHeader, signedToken(t, private, certs.URL, "unknown"))
		if verifier.Authenticate(request) {
			t.Fatal("unknown key ID authenticated")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("JWKS calls=%d want 1 for repeated unknown key ID", got)
	}
}

func signedToken(t *testing.T, private *rsa.PrivateKey, issuer, kid string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "ui-audience", "email": "admin@example.com", "exp": time.Now().Add(time.Hour).Unix()})
	token.Header["kid"] = kid
	raw, err := token.SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestStaleServesCached(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			entered <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "known", "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()), "e": "AQAB"}}})
	}))
	defer certs.Close()
	v, err := New(Config{TeamDomain: "example.com", AUD: "ui-audience", AllowedEmails: []string{"admin@example.com"}, Issuer: certs.URL, CertsURL: certs.URL, CacheTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(assertionHeader, signedToken(t, private, certs.URL, "known"))
	if !v.Authenticate(req) {
		t.Fatal("initial authentication failed")
	}
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-time.Minute - time.Second)
	v.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- v.Authenticate(req) }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("stale authentication failed")
		}
	case <-time.After(2 * time.Second):
		t.Error("stale authentication waited for refresh")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Error("refresh did not start")
	}
	close(release)
	waitRefresh(t, v)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d, want 2", got)
	}
}

func TestConcurrentRefreshOnce(t *testing.T) {
	private, _ := rsa.GenerateKey(rand.Reader, 2048)
	var calls atomic.Int32
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			entered <- struct{}{}
			<-release
		}
		writeTestKey(w, "known", private)
	}))
	defer server.Close()
	v := testVerifier(t, server.URL)
	if _, err := v.keyFor(context.Background(), "known"); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-2 * time.Minute)
	v.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.keyFor(context.Background(), "known"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d, want 2", got)
	}
	close(release)
	waitRefresh(t, v)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d, want 2", got)
	}
}

func TestUnknownKidFetches(t *testing.T) {
	old, _ := rsa.GenerateKey(rand.Reader, 2048)
	rotated, _ := rsa.GenerateKey(rand.Reader, 2048)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeTestKey(w, "old", old)
		} else {
			writeTestKey(w, "rotated", rotated)
		}
	}))
	defer server.Close()
	v := testVerifier(t, server.URL)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(assertionHeader, signedToken(t, old, server.URL, "old"))
	if !v.Authenticate(req) {
		t.Fatal("initial authentication failed")
	}
	v.mu.Lock()
	v.lastUnknownKidRefresh = time.Now().Add(-time.Minute)
	v.mu.Unlock()
	req.Header.Set(assertionHeader, signedToken(t, rotated, server.URL, "rotated"))
	if !v.Authenticate(req) {
		t.Fatal("rotated key did not authenticate")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d, want 2", got)
	}
}

func TestFailedRefreshKeepsKeys(t *testing.T) {
	private, _ := rsa.GenerateKey(rand.Reader, 2048)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		writeTestKey(w, "known", private)
	}))
	defer server.Close()
	v := testVerifier(t, server.URL)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(assertionHeader, signedToken(t, private, server.URL, "known"))
	if !v.Authenticate(req) {
		t.Fatal("initial authentication failed")
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-2 * time.Minute)
	v.mu.Unlock()
	if !v.Authenticate(req) {
		t.Fatal("stale key rejected")
	}
	waitRefresh(t, v)
	if !strings.Contains(logs.String(), "cfaccess.refresh_failed err=\"JWKS request failed\"") {
		t.Errorf("missing failure log: %s", logs.String())
	}
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-24*time.Hour - time.Second)
	v.mu.Unlock()
	if v.Authenticate(req) {
		t.Fatal("key older than 24 hours accepted after failed synchronous fetch")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls=%d, want 3", got)
	}
}

func writeTestKey(w http.ResponseWriter, kid string, private *rsa.PrivateKey) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": kid, "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()), "e": "AQAB"}}})
}

func testVerifier(t *testing.T, url string) *Verifier {
	t.Helper()
	v, err := New(Config{TeamDomain: "example.com", AUD: "ui-audience", AllowedEmails: []string{"admin@example.com"}, Issuer: url, CertsURL: url, CacheTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func waitRefresh(t *testing.T, v *Verifier) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v.mu.Lock()
		refreshing := v.refreshing
		v.mu.Unlock()
		if !refreshing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background refresh did not finish")
}
