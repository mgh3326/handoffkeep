package main

import (
	"strings"
	"testing"
)

func TestUITaskIssuesFromEnv(t *testing.T) {
	for _, tc := range []struct {
		source, url, token string
		on                 bool
		err                string
	}{
		{source: ""},
		{source: "hk", url: "https://yt.example.com", token: "x"},
		{source: "youtrack", url: "https://yt.example.com", token: "x", on: true},
		{source: "youtrack", err: "needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY"},
		{source: "YouTrack", url: "https://yt.example.com", token: "x", err: "want hk or youtrack"},
	} {
		t.Setenv("HANDOFFKEEP_UI_TASKS_SOURCE", tc.source)
		t.Setenv("YOUTRACK_URL", tc.url)
		t.Setenv("YOUTRACK_BOT_API_KEY", tc.token)
		client, err := uiTaskIssuesFromEnv()
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("source=%q: err=%v, want %q", tc.source, err, tc.err)
			}
			continue
		}
		if err != nil || (client != nil) != tc.on {
			t.Fatalf("source=%q: client=%v err=%v", tc.source, client != nil, err)
		}
		if tc.on && client.Tenant() != "yt.example.com" {
			t.Fatalf("tenant=%q", client.Tenant())
		}
	}
}
