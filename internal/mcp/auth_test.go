package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

const testAccessKid = "mcp-test-key"

type accessFixture struct {
	private  *rsa.PrivateKey
	issuer   string
	verifier *cfaccess.Verifier
}

func newAccessFixture(t *testing.T) accessFixture {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": testAccessKid, "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
			"e": "AQAB",
		}}})
	}))
	t.Cleanup(keys.Close)
	verifier, err := cfaccess.New(cfaccess.Config{
		TeamDomain:          "team.example",
		AUD:                 "mcp-audience",
		AllowedEmails:       []string{"operator@example.com"},
		AllowedServiceNames: []string{"mcp-service"},
		Issuer:              keys.URL,
		CertsURL:            keys.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accessFixture{private: private, issuer: keys.URL, verifier: verifier}
}

func (f accessFixture) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	base := jwt.MapClaims{
		"iss": f.issuer,
		"aud": "mcp-audience",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for key, value := range claims {
		base[key] = value
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	token.Header["kid"] = testAccessKid
	raw, err := token.SignedString(f.private)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func initializeBody() string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
}

func initialize(h http.Handler, setup func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if setup != nil {
		setup(request)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

// A local proxy (cloudflared) connects to /mcp over loopback while keeping
// the public Host header. Authentication runs before the SDK transport, so
// such a request is decided by its credential, not its Host.
func TestHTTPHandlerAcceptsPublicHostFromLocalProxy(t *testing.T) {
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{}, tokens, nil))
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		bearer bool
		want   int
	}{
		{"authenticated initialize", true, http.StatusOK},
		{"unauthenticated initialize", false, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initializeBody()))
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "hk-mcp.example.com"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			if tc.bearer {
				request.Header.Set("Authorization", "Bearer bearer-secret")
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.want {
				t.Fatalf("status=%d want %d body=%s", response.StatusCode, tc.want, body)
			}
		})
	}
}

func TestHTTPHandlerSecondCredential(t *testing.T) {
	fixture := newAccessFixture(t)
	tokens := api.Tokens{"tester": "bearer-secret"}
	assertion := func(raw string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Cf-Access-Jwt-Assertion", raw) }
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer bearer-secret") }
	for _, tc := range []struct {
		name   string
		access *cfaccess.Verifier
		setup  func(*http.Request)
		want   int
	}{
		{"bearer token", fixture.verifier, bearer, http.StatusOK},
		{"bearer token without access config", nil, bearer, http.StatusOK},
		{"access email", fixture.verifier, assertion(fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})), http.StatusOK},
		{"wrong audience", fixture.verifier, assertion(fixture.token(t, jwt.MapClaims{"email": "operator@example.com", "aud": "other-audience"})), http.StatusUnauthorized},
		{"expired", fixture.verifier, assertion(fixture.token(t, jwt.MapClaims{"email": "operator@example.com", "exp": time.Now().Add(-time.Hour).Unix()})), http.StatusUnauthorized},
		{"unlisted email", fixture.verifier, assertion(fixture.token(t, jwt.MapClaims{"email": "someone@example.com"})), http.StatusUnauthorized},
		{"service identity", fixture.verifier, assertion(fixture.token(t, jwt.MapClaims{"common_name": "mcp-service"})), http.StatusUnauthorized},
		{"access without config", nil, assertion(fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})), http.StatusUnauthorized},
		{"no credential", fixture.verifier, nil, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := initialize(HTTPHandler(api.Service{}, tokens, tc.access), tc.setup)
			if recorder.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				if got := recorder.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Fatalf("WWW-Authenticate=%q", got)
				}
				if got := recorder.Body.String(); got != `{"error":"unauthorized"}` {
					t.Fatalf("body=%q", got)
				}
			}
		})
	}
}
