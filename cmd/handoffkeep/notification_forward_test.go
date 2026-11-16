package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/hubforward"
)

// Turning forwarding on requires the instant it starts: without it the
// server refuses to start rather than flush the backlog or guess a cutoff
// (hk 4986).
func TestNotificationForwarderFromEnvRequiresSince(t *testing.T) {
	t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD", "on")
	t.Setenv("HANDOFFKEEP_HUB_URL", "https://hub.example")
	t.Setenv("HANDOFFKEEP_HUB_TOKEN", "hub-test-token")
	t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE", "")
	if _, err := notificationForwarderFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE") {
		t.Fatalf("unset err=%v", err)
	}
	for _, raw := range []string{"2026-11-17", "yesterday"} {
		t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE", raw)
		if _, err := notificationForwarderFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE") {
			t.Errorf("%q: err=%v", raw, err)
		}
	}
	t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE", "2026-11-17T00:00:00+09:00")
	w, err := notificationForwarderFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := w.(*hubforward.Forwarder)
	if !ok {
		t.Fatalf("worker=%T", w)
	}
	if want := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC); !f.Cutoff.Equal(want) {
		t.Fatalf("cutoff=%s, want %s", f.Cutoff, want)
	}
}

// Forwarding stays off by default, and off means no cutoff is required.
func TestNotificationForwarderFromEnvOff(t *testing.T) {
	t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD", "")
	t.Setenv("HANDOFFKEEP_NOTIFICATION_FORWARD_SINCE", "")
	if w, err := notificationForwarderFromEnv(nil); w != nil || err != nil {
		t.Fatalf("off: %v %v", w, err)
	}
}
