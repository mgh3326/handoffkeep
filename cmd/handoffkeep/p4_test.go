package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTasksTransitionDecisionOptionFlags(t *testing.T) {
	var input struct {
		To   string         `json:"to"`
		Note string         `json:"note"`
		Refs map[string]any `json:"refs"`
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/41/transition" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 41})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	if err := tasksCmd([]string{"transition", "41", "--to", "needs_decision", "--question", "저장 방식을 선택", "--option", "A|노드 로컬 저장", "--option", "B|중앙 저장 선행", "--recommended", "A", "--no-free-answer"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || input.To != "needs_decision" || input.Note != "저장 방식을 선택" || input.Refs == nil {
		t.Fatalf("calls=%d input=%+v", calls, input)
	}
	rawOptions, ok := input.Refs["decision_options"].(map[string]any)
	if !ok {
		t.Fatalf("decision options=%v", input.Refs)
	}
	rawItems, ok := rawOptions["options"].([]any)
	if !ok || len(rawItems) != 2 || rawOptions["allow_free"] != false {
		t.Fatalf("options=%v", rawOptions)
	}
	first, ok := rawItems[0].(map[string]any)
	second, secondOK := rawItems[1].(map[string]any)
	if !ok || !secondOK || first["recommended"] != true || second["recommended"] != nil {
		t.Fatalf("options=%+v", rawOptions)
	}

	invalid := [][]string{
		{"transition", "41", "--to", "needs_decision", "--question", "q", "--option", "Z|bad"},
		{"transition", "41", "--to", "needs_decision", "--question", "q", "--option", "A|one", "--option", "A|two"},
		{"transition", "41", "--to", "needs_decision", "--question", "q", "--option", "A|" + strings.Repeat("x", 121)},
		{"transition", "41", "--to", "needs_decision", "--question", "q", "--option", "A|one", "--recommended", "B"},
		{"transition", "41", "--to", "needs_decision", "--question", "q", "--recommended", "A"},
		{"transition", "41", "--to", "claimed", "--option", "A|one"},
	}
	tooMany := []string{"transition", "41", "--to", "needs_decision", "--question", "q"}
	for _, value := range []string{"A|one", "B|two", "C|three", "D|four", "E|five", "F|six", "A|seven"} {
		tooMany = append(tooMany, "--option", value)
	}
	invalid = append(invalid, tooMany)
	for _, args := range invalid {
		if err := tasksCmd(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid option flags made API calls=%d", calls)
	}
}
