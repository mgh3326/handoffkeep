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

// pemHeader and pemFooter build armor lines at runtime so gitleaks sees no
// key-shaped literal in this file.
func pemHeader(kind string) string { return "-----" + "BEGIN " + kind + "PRIVATE" + " KEY-----" }

func pemFooter(kind string) string { return "-----" + "END " + kind + "PRIVATE" + " KEY" + "-----" }

type pemKind struct {
	name string
	head []string
	tail []string
}

var pemKinds = []pemKind{
	{"pkcs8", []string{pemHeader("")}, []string{pemFooter("")}},
	{"rsa", []string{pemHeader("RSA ")}, []string{pemFooter("RSA ")}},
	{"ec", []string{pemHeader("EC ")}, []string{pemFooter("EC ")}},
	{"openssh", []string{pemHeader("OPENSSH ")}, []string{pemFooter("OPENSSH ")}},
	{"encrypted_rsa", []string{
		pemHeader("RSA "),
		"Proc-Type: 4,ENCRYPTED",
		"DEK-Info: AES-128-CBC," + strings.Repeat("0", 32),
		"",
	}, []string{pemFooter("RSA ")}},
	{"pgp", []string{
		"-----" + "BEGIN PGP PRIVATE" + " KEY BLOCK-----",
		"Version: GnuPG v2",
		"Comment: test",
		"",
	}, []string{"=AbCd", "-----" + "END PGP PRIVATE" + " KEY BLOCK-----"}},
}

var pemPrefixes = []struct{ name, text string }{
	{"gt", ">"},
	{"gt_space", "> "},
	{"gt_gt_space", "> > "},
	{"hash", "#"},
	{"hash_space", "# "},
	{"slash", "//"},
	{"slash_space", "// "},
	{"space_star_space", " * "},
}

const pemBody = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC" +
	"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC" +
	"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"

var pemBodyLines = []string{pemBody[:48], pemBody[48:96], pemBody[96:]}

// prefixJoin prefixes every line and joins with sep.
func prefixJoin(prefix, sep string, lines []string) string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = prefix + line
	}
	return strings.Join(out, sep)
}

// TestPrivateKeyRejectsPrefixedBodies covers a key whose header line and body
// lines are quoted or commented out (hk 3319): every prefix, armor kind and
// line ending must still be refused as private_key.
func TestPrivateKeyRejectsPrefixedBodies(t *testing.T) {
	endings := []struct {
		name string
		sep  string
		wrap func(string) string
	}{
		{"lf", "\n", func(s string) string { return s }},
		{"crlf", "\r\n", func(s string) string { return s }},
		{"escaped_lf", `\n`, func(s string) string { return `{"private_key": "` + s + `"}` }},
	}
	for _, p := range pemPrefixes {
		for _, k := range pemKinds {
			all := append(append(append([]string{}, k.head...), pemBodyLines...), k.tail...)
			for _, e := range endings {
				t.Run("all_lines/"+p.name+"/"+k.name+"/"+e.name, func(t *testing.T) {
					if got := Match(e.wrap(prefixJoin(p.text, e.sep, all))); got != "private_key" {
						t.Fatalf("prefixed key matched %q, want private_key", got)
					}
				})
				t.Run("body_only/"+p.name+"/"+k.name+"/"+e.name, func(t *testing.T) {
					input := strings.Join([]string{
						prefixJoin("", e.sep, k.head),
						prefixJoin(p.text, e.sep, pemBodyLines),
						prefixJoin("", e.sep, k.tail),
					}, e.sep)
					if got := Match(e.wrap(input)); got != "private_key" {
						t.Fatalf("prefixed body matched %q, want private_key", got)
					}
				})
			}
		}
	}
}

// TestPrivateKeyPrefixesAloneAreNotKeyMaterial keeps the #194 bounds: a
// prefixed header, a prefixed detector-regex quote and a prefixed padding-only
// body carry no key material.
func TestPrivateKeyPrefixesAloneAreNotKeyMaterial(t *testing.T) {
	padding := []string{pemHeader("RSA ")}
	for i := 0; i < 40; i++ {
		padding = append(padding, "=")
	}
	for _, p := range pemPrefixes {
		cases := map[string][]string{
			"prefixed header alone": {pemHeader("RSA ")},
			"prefixed regex quote":  {"re.compile(r'-----BEGIN (?:RSA |OPENSSH )?PRIVATE KEY-----')"},
		}
		// '/' is a base64 character, so "//=" padding lines already count as
		// body material on origin/main; bare "//" is not a padding-only case.
		if p.text != "//" {
			cases["prefixed padding body"] = padding
		}
		for name, lines := range cases {
			if got := Match(prefixJoin(p.text, "\n", lines)); got != "" {
				t.Fatalf("%s with prefix %q matched %q", name, p.text, got)
			}
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
