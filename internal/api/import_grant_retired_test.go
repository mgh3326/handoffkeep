package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A leftover HANDOFFKEEP_IMPORT_READONLY_ grant for a live token must stop
// startup: otherwise that token silently loses its import-only restriction.
// A grant for no live token is harmless and loads.
func TestLoadTokensRefusesLiveImportGrant(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "auth.env")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := write(t, "HANDOFFKEEP_TOKEN_youtrack=secret-one\nHANDOFFKEEP_TOKEN_oci=two\nHANDOFFKEEP_IMPORT_READONLY_youtrack=true\n")
	_, _, err := LoadTokens(p)
	if err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_IMPORT_READONLY_youtrack is retired") {
		t.Fatalf("live grant: err %v, want a refusal naming the grant", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret-one") {
		t.Fatalf("refusal carries the token: %v", err)
	}

	p = write(t, "HANDOFFKEEP_TOKEN_oci=two\nHANDOFFKEEP_IMPORT_READONLY_gone=true\n")
	tokens, _, err := LoadTokens(p)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("grant for no live token: tokens %v err %v, want the one token loaded", tokens, err)
	}
}
