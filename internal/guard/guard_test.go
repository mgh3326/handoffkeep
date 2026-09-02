package guard

import "testing"

func TestRejectsSecretsWithoutValue(t *testing.T) {
	for input, want := range map[string]string{
		"sk-abcdefghijk": "openai_key", "ghp_abcdefghijk": "github_token", "xoxb-abcdefghijk": "slack_token", "AKIAABCDEFGH": "aws_access_key", "-----BEGIN PRIVATE": "private_key", "Bearer abcdefghijklmnopqrstuvwxyz": "bearer_token", "MY_TOKEN=abcdefghijklm": "token_assignment", "github_pat_abcdefghijk": "github_fine_grained_token",
	} {
		if got := Match(input); got != want {
			t.Fatalf("input matched %q, want %q", got, want)
		}
	}
}

func TestRejectReturnsOnlyPatternName(t *testing.T) {
	err := Reject("Bearer abcdefghijklmnopqrstuvwxyz")
	if err == nil || err.Error() != "secret_like_content:bearer_token" {
		t.Fatalf("err=%v", err)
	}
}
