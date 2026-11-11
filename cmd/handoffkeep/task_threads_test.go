package main

import (
	"slices"
	"strings"
	"testing"
)

func TestTaskThreadPosterFromEnv(t *testing.T) {
	const secret = "tasks-webhook-credential-z1"
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "")
	t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", "")
	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_PROJECTS", "")

	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_WEBHOOK_URL", "")
	if p, err := taskThreadPosterFromEnv(nil); p != nil || err != nil {
		t.Fatalf("unset url: %+v %v", p, err)
	}

	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_WEBHOOK_URL", "http://discord.example/api/webhooks/1/"+secret)
	if _, err := taskThreadPosterFromEnv(nil); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("http url: err=%v", err)
	}

	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_WEBHOOK_URL", "https://discord.example/api/webhooks/1/"+secret)
	p, err := taskThreadPosterFromEnv(nil)
	if err != nil || p == nil || !slices.Equal(p.Projects, []string{"panewire", "handoffkeep", "mergewake", "rstack"}) || p.Store != nil {
		t.Fatalf("defaults: %+v %v", p, err)
	}

	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_PROJECTS", " handoffkeep , experiment,")
	op := "1234567890" + "12345678"
	t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", op)
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "https://console.example/")
	p, err = taskThreadPosterFromEnv(nil)
	if err != nil || !slices.Equal(p.Projects, []string{"handoffkeep", "experiment"}) || p.OperatorUserID != op || p.ConsoleURL != "https://console.example" {
		t.Fatalf("set: %+v %v", p, err)
	}

	for _, bad := range []string{",", "Has Space"} {
		t.Setenv("HANDOFFKEEP_DISCORD_TASKS_PROJECTS", bad)
		if _, err := taskThreadPosterFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_DISCORD_TASKS_PROJECTS") {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}
