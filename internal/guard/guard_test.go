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
		"-----BEGIN PRIVATE":                                    "private_key",
		"Bearer abcdefghijklmnopqrstuvwxyz":                     "bearer_token",
		"MY_TOKEN=abcdefghijklm":                                "token_assignment",
		"github_pat_" + strings.Repeat("a", 82):                 "github_fine_grained_token",
	} {
		if got := Match(input); got != want {
			t.Fatalf("input matched %q, want %q", got, want)
		}
	}
}

func TestAllowsJobIdentifiersThatResembleTokenPrefixes(t *testing.T) {
	for _, input := range []string{
		"sk-captain-verify10-20260904-1710",
		"sk-wrkinbox-20260904-1230",
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
