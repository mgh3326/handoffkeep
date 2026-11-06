package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPlacementRejectionNamesTheField pins that each refused placement record
// says which field failed, wrapped in ErrInvalidPlacement (the API maps it to
// 400 invalid_context with that text as reason).
func TestPlacementRejectionNamesTheField(t *testing.T) {
	host := "node-a"
	good := func() PlacementDecision {
		return PlacementDecision{JobID: "job-1", Attempt: 1, DecidedOn: host, DecidedAt: time.Now().UTC(), Source: "auto", Outcome: "refused", Ranking: "score", StateSource: "hub", Candidates: []PlacementCandidate{}}
	}
	if err := checkPlacement(good()); err != nil {
		t.Fatalf("good record refused: %v", err)
	}
	for _, tc := range []struct {
		want string
		mut  func(*PlacementDecision)
	}{
		{"candidates missing", func(d *PlacementDecision) { d.Candidates = nil }},
		{"role", func(d *PlacementDecision) { d.Role = "tester" }},
		{"decided_at older than retention", func(d *PlacementDecision) { d.DecidedAt = time.Now().UTC().Add(-PlacementRetention - time.Hour) }},
		{"candidates[0].host", func(d *PlacementDecision) { d.Candidates = []PlacementCandidate{{Host: "", Outcome: "x"}} }},
		{"reason", func(d *PlacementDecision) { d.Reason = "a\x00b" }},
	} {
		d := good()
		tc.mut(&d)
		err := checkPlacement(d)
		if !errors.Is(err, ErrInvalidPlacement) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v", tc.want, err)
		}
	}
}
