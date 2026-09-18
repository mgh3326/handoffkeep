package ui

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestIsDecisionEscalation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"decision", "[decision-needed] choose", true},
		{"leading space decision", "   [decision-needed] choose", true},
		{"leading newline decision", "\n\t[decision-needed] choose", true},
		{"marker only", "[decision-needed]", true},
		{"ordinary old status", "worker-a status ok", false},
		{"ready", "READY", false},
		{"ignored", "ordinary question (ignore)", false},
		{"plain question", "Which option should we use?", false},
		{"mid marker", "status [decision-needed] mid", false},
		{"esc prefix marker", "ESC: pick [decision-needed]", false},
		{"case differs", "[Decision-Needed] choose", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isDecisionEscalation(store.RelayEvent{Question: test.text}); got != test.want {
				t.Fatalf("isDecisionEscalation(%q)=%t, want %t", test.text, got, test.want)
			}
		})
	}
	// The effective-message fallback precedence Question > Text >
	// ReportLastLine is preserved for the marker check.
	if !isDecisionEscalation(store.RelayEvent{Text: "  [decision-needed] via text"}) || !isDecisionEscalation(store.RelayEvent{ReportLastLine: "[decision-needed] via last line"}) {
		t.Fatal("classification did not use text and report fallbacks")
	}
	if isDecisionEscalation(store.RelayEvent{Question: "plain", Text: "[decision-needed] hidden", ReportLastLine: "[decision-needed] hidden"}) {
		t.Fatal("classification skipped the question field's precedence")
	}
}

func TestDecisionEscalationQuestion(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{"decision", "[decision-needed] choose", "choose"},
		{"leading space", "   [decision-needed]   choose  ", "choose"},
		{"unmarked passthrough", "plain question", "plain question"},
		{"mid marker preserved", "a [decision-needed] b", "a [decision-needed] b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decisionEscalationQuestion(store.RelayEvent{Question: test.text}); got != test.want {
				t.Fatalf("decisionEscalationQuestion(%q)=%q, want %q", test.text, got, test.want)
			}
		})
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
		{"escalation", store.RelayEvent{ID: 1, Question: "[decision-needed] Choose\noptions: yes | no"}},
		{"lane", store.RelayEvent{ID: 2, Text: "[decision-needed] options: yes | no"}},
	} {
		form := eventFormData(test.kind, test.event, "csrf", true)
		if len(form.Options) != 2 || form.Options[0] != "yes" || form.Options[1] != "no" {
			t.Fatalf("%s options=%q", test.kind, form.Options)
		}
	}
}
