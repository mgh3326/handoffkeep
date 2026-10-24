package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// validAreaException is the minimal row that passes every field check: one
// evidence ref per accepted shape is covered separately below.
func validAreaException() BenchAreaException {
	return BenchAreaException{
		Profile:            "devin-swe2",
		Effort:             "high",
		Area:               "blender",
		Action:             "exclude",
		EvidenceRefs:       []string{"hk:task/1836"},
		DecisionRef:        "hk:doc/decision/2026-10-24/area-exceptions-1",
		DecidedBy:          "operator",
		DecidedAt:          time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC),
		ExpiresWithModelID: "devin-swe2-2026-10",
	}
}

// TestBenchAreaExceptionValidation covers every refusal reason of
// checkBenchAreaException. Each case names the mutant it keeps dead: a store
// that drops the check fails exactly the row that relies on it.
func TestBenchAreaExceptionValidation(t *testing.T) {
	longRef := "hk:doc/" + strings.Repeat("k", 600)
	for _, tc := range []struct {
		name   string
		edit   func(*BenchAreaException)
		want   string
		wantIs error
	}{
		{"empty profile", func(x *BenchAreaException) { x.Profile = "" }, "profile", nil},
		{"oversized profile", func(x *BenchAreaException) { x.Profile = strings.Repeat("p", 201) }, "profile", nil},
		{"profile with NUL", func(x *BenchAreaException) { x.Profile = "a\x00b" }, "profile", nil},
		{"oversized effort", func(x *BenchAreaException) { x.Effort = strings.Repeat("e", 201) }, "effort", nil},
		{"unknown area", func(x *BenchAreaException) { x.Area = "go" }, "", ErrTaskAreaUnknown},
		{"area case miss", func(x *BenchAreaException) { x.Area = "Blender" }, "", ErrTaskAreaUnknown},
		{"empty area", func(x *BenchAreaException) { x.Area = "" }, "", ErrTaskAreaUnknown},
		{"bad action", func(x *BenchAreaException) { x.Action = "require" }, "action", nil},
		{"empty action", func(x *BenchAreaException) { x.Action = "" }, "action", nil},
		{"no evidence", func(x *BenchAreaException) { x.EvidenceRefs = nil }, "evidence", nil},
		{"empty evidence", func(x *BenchAreaException) { x.EvidenceRefs = []string{} }, "evidence", nil},
		{"too many refs", func(x *BenchAreaException) {
			x.EvidenceRefs = make([]string, benchAreaExceptionMaxRefs+1)
			for i := range x.EvidenceRefs {
				x.EvidenceRefs[i] = "srv:1"
			}
		}, "evidence", nil},
		{"task ref not a number", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:task/abc"} }, "evidence", nil},
		{"task ref empty", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:task/"} }, "evidence", nil},
		{"task ref trailing", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:task/42x"} }, "evidence", nil},
		{"task ref padded", func(x *BenchAreaException) { x.EvidenceRefs = []string{" hk:task/42"} }, "evidence", nil},
		{"doc ref empty key", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:doc/"} }, "evidence", nil},
		{"doc ref leading slash", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:doc//abs"} }, "evidence", nil},
		{"doc ref dotdot", func(x *BenchAreaException) { x.EvidenceRefs = []string{"hk:doc/a/../b"} }, "evidence", nil},
		{"doc ref oversized", func(x *BenchAreaException) { x.EvidenceRefs = []string{longRef} }, "evidence", nil},
		{"srv ref not a number", func(x *BenchAreaException) { x.EvidenceRefs = []string{"srv:x"} }, "evidence", nil},
		{"srv ref empty", func(x *BenchAreaException) { x.EvidenceRefs = []string{"srv:"} }, "evidence", nil},
		{"unknown ref scheme", func(x *BenchAreaException) { x.EvidenceRefs = []string{"local:12"} }, "evidence", nil},
		{"bare number ref", func(x *BenchAreaException) { x.EvidenceRefs = []string{"1031"} }, "evidence", nil},
		{"ref with space", func(x *BenchAreaException) { x.EvidenceRefs = []string{"srv:1 2"} }, "evidence", nil},
		{"empty decision_ref", func(x *BenchAreaException) { x.DecisionRef = "" }, "", ErrDecisionRefRequired},
		{"blank decision_ref", func(x *BenchAreaException) { x.DecisionRef = "   " }, "", ErrDecisionRefRequired},
		{"empty model id", func(x *BenchAreaException) { x.ExpiresWithModelID = "" }, "expires_with_model_id", nil},
		{"empty decided_by", func(x *BenchAreaException) { x.DecidedBy = "" }, "", ErrDecidedByRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := validAreaException()
			tc.edit(&x)
			err := checkBenchAreaException(x)
			if err == nil {
				t.Fatalf("accepted %+v", x)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want text %q", err, tc.want)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err=%v, want errors.Is %v", err, tc.wantIs)
			}
		})
	}
}

// TestBenchAreaExceptionRefShapes accepts exactly the three evidence ref
// shapes: hk:task/N, hk:doc/KEY and srv:N.
func TestBenchAreaExceptionRefShapes(t *testing.T) {
	for _, ref := range []string{
		"hk:task/1", "hk:task/1944",
		"hk:doc/decision/2026-10-24/area-exceptions-1", "hk:doc/reports/b953/hk1938-kind-grading-packet",
		"srv:1", "srv:1031",
	} {
		x := validAreaException()
		x.EvidenceRefs = []string{ref}
		if err := checkBenchAreaException(x); err != nil {
			t.Fatalf("ref %q refused: %v", ref, err)
		}
	}
	// Every action in the closed set passes; '' effort means every effort of
	// the profile and must validate.
	for _, action := range []string{"exclude", "prefer"} {
		x := validAreaException()
		x.Action = action
		if err := checkBenchAreaException(x); err != nil {
			t.Fatalf("action %q refused: %v", action, err)
		}
	}
	x := validAreaException()
	x.Effort = ""
	if err := checkBenchAreaException(x); err != nil {
		t.Fatalf("wildcard effort refused: %v", err)
	}
	// Every vocabulary area is accepted.
	for _, area := range TaskAreas() {
		x := validAreaException()
		x.Area = area
		if err := checkBenchAreaException(x); err != nil {
			t.Fatalf("area %q refused: %v", area, err)
		}
	}
}
