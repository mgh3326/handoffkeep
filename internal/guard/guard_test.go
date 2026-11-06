package guard

import (
	"strings"
	"testing"
)

func TestRejectsSecretsWithoutValue(t *testing.T) {
	for input, want := range map[string]string{
		"sk-" + strings.Repeat("a", 48):                         "openai_key",
		"sk-proj-" + strings.Repeat("A", 40):                    "openai_key",
		"sk-ant-api03-" + strings.Repeat("a", 40):               "anthropic_key",
		"ghp_" + strings.Repeat("a", 36):                        "github_token",
		"xoxb-1234567890-1234567890-" + strings.Repeat("a", 20): "slack_token",
		"AKIA" + strings.Repeat("A", 16):                        "aws_access_key",
		"Bearer abcdefghijklmnopqrstuvwxyz":                     "bearer_token",
		"MY_TOKEN=abcdefghijklm":                                "token_assignment",
		"github_pat_" + strings.Repeat("a", 82):                 "github_fine_grained_token",
	} {
		if got := Match(input); got != want {
			t.Fatalf("input matched %q, want %q", got, want)
		}
	}
}

func TestPrivateKeyNeedsKeyMaterial(t *testing.T) {
	body := strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASC", 3)
	// Headers are assembled at runtime so gitleaks sees no key-shaped literal.
	pem := func(kind string) string { return "-----" + "BEGIN " + kind + "PRIVATE" + " KEY-----" }
	for _, header := range []string{pem(""), pem("RSA "), pem("OPENSSH "), pem("EC ")} {
		if got := Match(header + "\n" + body + "\n-----END" + " PRIVATE KEY-----"); got != "private_key" {
			t.Fatalf("%s with a body matched %q, want private_key", header, got)
		}
	}
	// Legacy encrypted PEM: Proc-Type and DEK-Info lines precede the body.
	encrypted := pem("RSA ") + "\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC," + strings.Repeat("0", 32) + "\n\n" + body + "\n-----END" + " RSA PRIVATE KEY-----"
	if got := Match(encrypted); got != "private_key" {
		t.Fatalf("encrypted PEM matched %q, want private_key", got)
	}
	// OpenPGP armored private key block with armor headers (review r: CodeRabbit Major).
	pgp := "-----" + "BEGIN PGP PRIVATE" + " KEY BLOCK-----\nVersion: GnuPG v2\nComment: test\n\n" + body + "\n=AbCd\n-----END PGP PRIVATE" + " KEY BLOCK-----"
	if got := Match(pgp); got != "private_key" {
		t.Fatalf("PGP private key block matched %q, want private_key", got)
	}
	// JSON-escaped PEM: literal backslash-n separators, as in a service-account key file.
	escaped := `{"private_key": "` + pem("") + `\n` + body[:48] + `\n` + body[48:] + `\n-----END` + ` PRIVATE KEY-----\n"}`
	if got := Match(escaped); got != "private_key" {
		t.Fatalf("JSON-escaped PEM matched %q, want private_key", got)
	}
	// JSON-escaped forms with header lines: legacy encrypted RSA and PGP armor (lead verifier).
	for name, in := range map[string]string{
		"escaped encrypted RSA": pem("RSA ") + `\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,` + strings.Repeat("0", 32) + `\n\n` + body + `\n-----END` + ` RSA PRIVATE KEY-----`,
		"escaped PGP armor":     "-----" + "BEGIN PGP PRIVATE" + ` KEY BLOCK-----\nVersion: GnuPG v2\nComment: test\n\n` + body + `\n=AbCd\n-----END PGP PRIVATE` + " KEY BLOCK-----",
		"escaped CRLF RSA":      pem("RSA ") + `\r\nProc-Type: 4,ENCRYPTED\r\nDEK-Info: AES-128-CBC,` + strings.Repeat("0", 32) + `\r\n\r\n` + body,
	} {
		if got := Match(in); got != "private_key" {
			t.Fatalf("%s matched %q, want private_key", name, got)
		}
	}
	// Padding and line breaks alone do not make key material (minor review note).
	if got := Match(pem("") + "\n" + strings.Repeat("=\n", 40)); got != "" {
		t.Fatalf("padding-only body matched %q", got)
	}
	// A detector's own pattern text, a bare header and a public key are not key material.
	for _, input := range []string{
		"re.compile(r'-----BEGIN (?:RSA |OPENSSH )?PRIVATE KEY-----')",
		pem(""),
		pem("") + " placeholder",
		"-----BEGIN PUBLIC KEY-----\n" + body,
		"-----BEGIN CERTIFICATE-----\n" + body,
	} {
		if got := Match(input); got != "" {
			t.Fatalf("%q matched %q", input, got)
		}
	}
}

func TestAllowsJobIdentifiersThatResembleTokenPrefixes(t *testing.T) {
	for _, input := range []string{
		"sk-captain-verify10-20260904-1710",
		"sk-jobinbox-20260904-1230",
		"task-sk-2026",
		"risk-20260904-1230",
		"desk-captain-20260904-1230",
		"SK-CAPTAIN-VERIFY10-20260904-1710",
	} {
		if got := Match(input); got != "" {
			t.Fatalf("job identifier %q matched %q", input, got)
		}
	}
}

func TestRejectReturnsOnlyPatternName(t *testing.T) {
	err := Reject("Bearer abcdefghijklmnopqrstuvwxyz")
	if err == nil || err.Error() != "secret_like_content:bearer_token" {
		t.Fatalf("err=%v", err)
	}
}
