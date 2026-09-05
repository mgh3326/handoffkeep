package cfaccess

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
