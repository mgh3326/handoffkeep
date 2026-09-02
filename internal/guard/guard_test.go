package guard

import "testing"

func TestRejectsSecretsWithoutValue(t *testing.T) {
	if got := Match("Bearer abcdefghijklmnopqrstuvwxyz"); got != "bearer_token" {
		t.Fatalf("got %q", got)
	}
}
