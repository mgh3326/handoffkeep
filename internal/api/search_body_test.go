package api

import (
	"encoding/json"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// An empty page after a deadline carries partial and timed_out at the response
// level, where no row can, so it reads differently from no match.
func TestSearchBodyMarksEmptyTimedOutPage(t *testing.T) {
	body := SearchBody(store.SearchPage{Results: []store.SearchResult{}, Partial: true, TimedOut: true})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Results  []store.SearchResult `json:"results"`
		Partial  bool                 `json:"partial"`
		TimedOut bool                 `json:"timed_out"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 0 || !got.Partial || !got.TimedOut {
		t.Fatalf("body = %s, want no results marked partial and timed_out", raw)
	}
}

// A complete page keeps the response it had before: no marker keys at all.
func TestSearchBodyOmitsMarkersOnCompletePage(t *testing.T) {
	body := SearchBody(store.SearchPage{Results: []store.SearchResult{{Scope: "docs", Key: "design/x"}}})
	if _, ok := body["partial"]; ok {
		t.Fatalf("complete page carries partial: %v", body)
	}
	if _, ok := body["timed_out"]; ok {
		t.Fatalf("complete page carries timed_out: %v", body)
	}
	if _, ok := body["results"]; !ok {
		t.Fatalf("body = %v, want results", body)
	}
}
