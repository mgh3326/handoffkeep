package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestLanesLookCommand pins the lane look request (operator decision 418,
// hk 5201): the lane in the path, the notice event id in the body, the note
// optional, and a missing notice refused.
func TestLanesLookCommand(t *testing.T) {
	server, last := fakeHK(t, map[string]any{
		"changed": true,
		"look":    store.LaneLook{ID: 1, Lane: "handoffkeep-design", NoticeEventID: "lanecheck-handoffkeep-design-abcdef01-428000"},
	})
	var out, errout bytes.Buffer
	base := []string{"--url", server.URL, "--token", "t"}
	if err := run(append([]string{"lanes", "look", "handoffkeep-design", "--notice", "lanecheck-handoffkeep-design-abcdef01-428000", "--note", "checked"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Method != "POST" || last.Path != "/v1/lanes/handoffkeep-design/looks" ||
		!strings.Contains(last.Body, `"notice":"lanecheck-handoffkeep-design-abcdef01-428000"`) ||
		!strings.Contains(last.Body, `"note":"checked"`) {
		t.Fatalf("look request: %+v", *last)
	}
	for _, bad := range [][]string{
		{"lanes", "look", "handoffkeep-design"},
		{"lanes", "look"},
		{"lanes", "look", "handoffkeep-design", "extra", "--notice", "x"},
		{"lanes", "other"},
	} {
		if err := run(append(bad, base...), &out, &errout); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}
