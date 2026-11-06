package store

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func i64(v int) *int         { return &v }

func placementWithCandidate(c PlacementCandidate) PlacementDecision {
	return PlacementDecision{JobID: "job-v2", Attempt: 1, DecidedOn: "node-a", DecidedAt: time.Now().UTC().Truncate(time.Microsecond), Source: "auto", Outcome: "refused", Ranking: "score", StateSource: "hub", Candidates: []PlacementCandidate{c}}
}

func TestPlacementScoreV2InputsValidation(t *testing.T) {
	ok := PlacementCandidate{Host: "node-a", Outcome: "ok", LiveJobs: i64(0), Load1PerCore: f64(0), Terms: &PlacementTerms{CPU: 0, Mem: 1, IO: 0.5, Load: 0.25, Jobs: 1}}
	if err := checkPlacement(placementWithCandidate(ok)); err != nil {
		t.Fatalf("valid score-v2 candidate refused: %v", err)
	}
	for _, tc := range []struct {
		want string
		mut  func(*PlacementCandidate)
	}{
		{"candidates[0].live_jobs", func(c *PlacementCandidate) { c.LiveJobs = i64(-1) }},
		{"candidates[0].load1_per_core", func(c *PlacementCandidate) { c.Load1PerCore = f64(-0.1) }},
		{"candidates[0].load1_per_core", func(c *PlacementCandidate) { c.Load1PerCore = f64(math.NaN()) }},
		{"candidates[0].load1_per_core", func(c *PlacementCandidate) { c.Load1PerCore = f64(math.Inf(1)) }},
		{"candidates[0].terms.cpu", func(c *PlacementCandidate) { c.Terms.CPU = -0.1 }},
		{"candidates[0].terms.mem", func(c *PlacementCandidate) { c.Terms.Mem = 1.5 }},
		{"candidates[0].terms.io", func(c *PlacementCandidate) { c.Terms.IO = math.NaN() }},
		{"candidates[0].terms.load", func(c *PlacementCandidate) { c.Terms.Load = math.Inf(-1) }},
		{"candidates[0].terms.jobs", func(c *PlacementCandidate) { c.Terms.Jobs = 2 }},
	} {
		c := ok
		terms := *ok.Terms
		c.Terms = &terms
		tc.mut(&c)
		err := checkPlacement(placementWithCandidate(c))
		if !errors.Is(err, ErrInvalidPlacement) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v", tc.want, err)
		}
	}
}

func TestPlacementScoreV2InputsRoundTrip(t *testing.T) {
	s := schemaStore(t, "placement_v2")
	c := PlacementCandidate{Host: "node-a", Outcome: "ok", Score: f64(0.42), LiveJobs: i64(3), Load1PerCore: f64(0.75), Terms: &PlacementTerms{CPU: 0.1, Mem: 0.2, IO: 0.3, Load: 0.4, Jobs: 0.5}}
	d := placementWithCandidate(c)
	d.JobID = "job-v2-roundtrip"
	stored, created, err := s.RecordPlacement(t.Context(), d)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	list, err := s.ListPlacements(t.Context(), d.JobID, "", time.Now().UTC().Add(-time.Hour), 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	for _, got := range []PlacementDecision{stored, list[0]} {
		if !reflect.DeepEqual(got.Candidates, []PlacementCandidate{c}) {
			t.Fatalf("candidates=%+v want %+v", got.Candidates, c)
		}
	}
	// A retry of the identical record carrying the new fields is not a conflict.
	if _, created, err = s.RecordPlacement(t.Context(), d); err != nil || created {
		t.Fatalf("retry created=%v err=%v", created, err)
	}
}
