package guard

import (
	"fmt"
	"testing"
)

func TestRejectReturnsSecretError(t *testing.T) {
	err := Reject("token sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz")
	if err == nil || err.Error() != "secret_like_content:anthropic_key" {
		t.Fatalf("err=%v", err)
	}
	if p, ok := SecretPattern(fmt.Errorf("put document: %w", err)); !ok || p != "anthropic_key" {
		t.Fatalf("pattern=%q ok=%v through a wrap", p, ok)
	}
	if _, ok := SecretPattern(fmt.Errorf("secret_like_content:openai_key")); ok {
		t.Fatal("matched on text instead of type")
	}
	if Reject("plain text") != nil {
		t.Fatal("plain text refused")
	}
}
