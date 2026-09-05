package ui

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestIsSignalEscalation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"ping", " PING ok", true},
		{"lane ok", "LANE-OK: done", true},
		{"lane fail", "lane-fail, retry", true},
		{"ready", "READY", true},
		{"bounce", "BOUNCE now", true},
		{"merged cleanup", "MERGED-CLEANUP", true},
		{"fleet ok", "FLEET-OK.", true},
		{"fleet fail", "FLEET-FAIL!", true},
		{"lost relay", "LOST-RELAY", true},
		{"report", "REPORT: complete", true},
		{"word boundary", "READYFOO", false},
		{"ignored", "ordinary question (ignore)", true},
		{"escalation", "ESC choose a path", false},
		{"decision", "[decision-needed] choose", false},
		{"question", "Which option should we use?", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isSignalEscalation(store.RelayEvent{Question: test.text}); got != test.want {
				t.Fatalf("isSignalEscalation(%q)=%t, want %t", test.text, got, test.want)
			}
		})
	}
	if !isSignalEscalation(store.RelayEvent{Text: "PING fallback"}) || !isSignalEscalation(store.RelayEvent{ReportLastLine: "REPORT fallback"}) {
		t.Fatal("classification did not use text and report fallbacks")
	}
}

func TestDecisionOptions(t *testing.T) {
	options := decisionOptions("Which?\noptions: approve | hold | reject | | ")
	if len(options) != 3 || options[0] != "approve" || options[2] != "reject" {
		t.Fatalf("options=%q", options)
	}
	if got := decisionOptions("No choices here"); len(got) != 0 {
		t.Fatalf("got options without options line: %q", got)
	}
}

func TestEventDecisionOptions(t *testing.T) {
	for _, test := range []struct {
		kind  string
		event store.RelayEvent
	}{
		{"escalation", store.RelayEvent{ID: 1, Question: "Choose\noptions: yes | no"}},
		{"lane", store.RelayEvent{ID: 2, Text: "[decision-needed] options: yes | no"}},
	} {
		form := eventFormData(test.kind, test.event, "csrf", true)
		if len(form.Options) != 2 || form.Options[0] != "yes" || form.Options[1] != "no" {
			t.Fatalf("%s options=%q", test.kind, form.Options)
		}
	}
}
