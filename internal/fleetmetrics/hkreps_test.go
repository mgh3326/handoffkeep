package fleetmetrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// benchHK serves only bench reps; every other HK method is unused here.
type benchHK struct {
	HK
	rows  []store.BenchRep
	err   error
	limit int
}

func (b *benchHK) ListBenchReps(_ context.Context, limit int) ([]store.BenchRep, error) {
	b.limit = limit
	return b.rows, b.err
}

func TestCollectRepsReadsHKBenchReps(t *testing.T) {
	ref, role, rounds := "646", "impl", 3
	at := time.Date(2026, 9, 24, 7, 48, 41, 0, time.UTC)
	hk := &benchHK{rows: []store.BenchRep{{ID: 921, Profile: "builder-opus", TaskRef: &ref, Role: &role, Rounds: &rounds, RecordedAt: at}, {ID: 7, Profile: "opus"}}}
	var s Snapshot
	collectReps(context.Background(), CollectConfig{HK: hk, HKReps: true}, &s)
	if hk.limit != hkRepsLimit || len(s.RepSources) != 1 || s.RepSources[0] != "hk bench_reps" || len(s.Notes) != 0 {
		t.Fatalf("limit=%d sources=%v notes=%v", hk.limit, s.RepSources, s.Notes)
	}
	if len(s.Reps) != 2 || s.Reps[0].Task != "646" || s.Reps[0].Role != "impl" || *s.Reps[0].Rounds != 3 || !s.Reps[0].RecordedAt.Equal(at) || s.Reps[1].Task != "" {
		t.Fatalf("reps=%+v", s.Reps)
	}

	// A failure is a note, never a source; a full page says older reps are cut.
	s = Snapshot{}
	collectReps(context.Background(), CollectConfig{HK: &benchHK{err: errors.New("hk down")}, HKReps: true}, &s)
	if len(s.RepSources) != 0 || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "hk down") {
		t.Fatalf("error: sources=%v notes=%v", s.RepSources, s.Notes)
	}
	s = Snapshot{}
	collectReps(context.Background(), CollectConfig{HK: &benchHK{rows: make([]store.BenchRep, hkRepsLimit)}, HKReps: true}, &s)
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "older reps are missing") {
		t.Fatalf("full page: notes=%v", s.Notes)
	}
	s = Snapshot{}
	collectReps(context.Background(), CollectConfig{HK: &benchHK{err: errors.New("unused")}}, &s)
	if len(s.Notes) != 0 || len(s.RepSources) != 0 {
		t.Fatalf("--no-hk-reps still read hk: %+v", s)
	}
}
