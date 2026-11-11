package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
)

func TestTaskThreadPosterFromEnv(t *testing.T) {
	const secret = "tasks-bot-credential-z1"
	const forum = "700000000000000001"
	t.Setenv("HANDOFFKEEP_CONSOLE_URL", "")
	t.Setenv("HANDOFFKEEP_DISCORD_OPERATOR_USER_ID", "")
	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_PROJECTS", "")

	// Both unset: off.
	t.Setenv("HANDOFFKEEP_DISCORD_BOT_TOKEN", "")
	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_FORUM_ID", "")
	if p, err := taskThreadPosterFromEnv(nil); p != nil || err != nil {
		t.Fatalf("both unset: %+v %v", p, err)
	}

	// Exactly one set, or a bad forum id: refused, naming the variable and
	// quoting no value.
	for _, tc := range []struct{ token, forum, names string }{
		{secret, "", "HANDOFFKEEP_DISCORD_TASKS_FORUM_ID"},
		{"", forum, "HANDOFFKEEP_DISCORD_BOT_TOKEN"},
		{secret, "not-a-snowflake-" + secret, "HANDOFFKEEP_DISCORD_TASKS_FORUM_ID"},
		{secret, "12/34", "HANDOFFKEEP_DISCORD_TASKS_FORUM_ID"},
	} {
		t.Setenv("HANDOFFKEEP_DISCORD_BOT_TOKEN", tc.token)
		t.Setenv("HANDOFFKEEP_DISCORD_TASKS_FORUM_ID", tc.forum)
		p, err := taskThreadPosterFromEnv(nil)
		if p != nil || err == nil || !strings.HasPrefix(err.Error(), tc.names) || strings.Contains(err.Error(), secret) || (tc.forum != "" && strings.Contains(err.Error(), tc.forum)) {
			t.Fatalf("token=%v forum=%q: %+v err=%v", tc.token != "", tc.forum, p, err)
		}
	}

	// Both set: the poster runs as the bot in that forum.
	t.Setenv("HANDOFFKEEP_DISCORD_BOT_TOKEN", secret)
	t.Setenv("HANDOFFKEEP_DISCORD_TASKS_FORUM_ID", " "+forum+" ")
	p, err := taskThreadPosterFromEnv(nil)
	if err != nil || p == nil || !slices.Equal(p.Projects, []string{"panewire", "handoffkeep", "mergewake", "rstack"}) || p.Store != nil || p.ForumID != forum {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	if bot, ok := p.Discord.(*api.DiscordBot); !ok || bot.Token != secret {
		t.Fatalf("discord client %T", p.Discord)
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
