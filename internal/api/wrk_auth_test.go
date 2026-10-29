package api

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestWrkAuthRejectsAmbiguousIdentity(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(a); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	x, y := hex.EncodeToString(a), hex.EncodeToString(b)
	for name, body := range map[string]string{
		"duplicate_client":  "HANDOFFKEEP_TOKEN_desktop=" + x + "\nHANDOFFKEEP_TOKEN_desktop=" + y + "\n",
		"equal_credentials": "HANDOFFKEEP_TOKEN_desktop=" + x + "\nHANDOFFKEEP_TOKEN_mac-dev=" + x + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "synthetic-auth.txt")
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadTokens(p); err == nil {
				t.Fatal("ambiguous authenticated identity accepted")
			}
		})
	}
}
