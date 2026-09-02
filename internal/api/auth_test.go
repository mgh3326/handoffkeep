package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenFileAndBearerIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.env")
	if err := os.WriteFile(p, []byte("HANDOFFKEEP_TOKEN_mac-personal=one\nHANDOFFKEEP_TOKEN_oci=two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tokens, err := LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer two")
	if id, ok := tokens.Client(r); !ok || id != "oci" {
		t.Fatalf("id=%q ok=%v", id, ok)
	}
	if _, ok := tokens.Client(httptest.NewRequest("GET", "/", nil)); ok {
		t.Fatal("missing token accepted")
	}
}
