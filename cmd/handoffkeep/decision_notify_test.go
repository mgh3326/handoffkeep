package main

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
)

// The console link is optional, but a value that is not https stops the
// server instead of silently dropping every link.
func TestDecisionNotifierFromEnv(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL", "")
	for _, raw := range []string{"http://console.example", "console.example/ui"} {
		t.Setenv("HANDOFFKEEP_CONSOLE_URL", raw)
		if _, err := decisionNotifierFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_CONSOLE_URL") {
			t.Errorf("%q: err=%v", raw, err)
		}
	}
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "https://console.example/")
	w, err := decisionNotifierFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := w.(api.DecisionNotifier); !ok || n.ConsoleURL != "https://console.example" || n.Discord != nil {
		t.Fatalf("notifier=%+v", w)
	}
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "")
	if w, err = decisionNotifierFromEnv(nil); err != nil || w.(api.DecisionNotifier).ConsoleURL != "" {
		t.Fatalf("unset: %+v %v", w, err)
	}
}

// The operator id is optional, but an invalid one stops the server, and the
// message never echoes the value.
func TestDecisionNotifierOperatorUserIDFromEnv(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL", "")
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "")
	op := "1234567890" + "12345678"
	for _, raw := range []string{"12345", "<@" + op + ">", op + "x"} {
		t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", raw)
		_, err := decisionNotifierFromEnv(nil)
		if err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_DISCORD_OPERATOR_USER_ID") || strings.Contains(err.Error(), raw) {
			t.Errorf("%q: err=%v", raw, err)
		}
	}
	t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", " "+op+" ")
	w, err := decisionNotifierFromEnv(nil)
	if err != nil || w.(api.DecisionNotifier).OperatorUserID != op {
		t.Fatalf("valid: %+v %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", "")
	if w, err = decisionNotifierFromEnv(nil); err != nil || w.(api.DecisionNotifier).OperatorUserID != "" {
		t.Fatalf("unset: %+v %v", w, err)
	}
}
