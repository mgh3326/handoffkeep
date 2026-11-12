package ytwrite_test

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The operator's view never calls an uncertain write done.
func TestViewStatus(t *testing.T) {
	for state, want := range map[ytwrite.State]ytwrite.Status{
		ytwrite.StatePrepared:      ytwrite.StatusPending,
		ytwrite.StateDispatched:    ytwrite.StatusPending,
		ytwrite.StateRemoteUnknown: ytwrite.StatusPending,
		ytwrite.StateApplied:       ytwrite.StatusApplied,
		ytwrite.StateRejected:      ytwrite.StatusRejected,
	} {
		if got := ytwrite.StatusOf(state); got != want {
			t.Errorf("StatusOf(%s) = %q, want %q", state, got, want)
		}
	}
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	v := ytwrite.ViewOf(ytwrite.Op{ID: 9, Kind: ytwrite.KindCommentAppend, EntityKind: "task", EntityID: "7", YTTargetID: "HK-7", Actor: "w",
		State: ytwrite.StateRemoteUnknown, Reason: ytwrite.ReasonNoEvidence, LastError: "youtrack answered 500: boom", Attempts: 1, Reconciles: 2, CreatedAt: at, NextAttemptAt: at.Add(time.Minute),
		Payload: []byte(`{"body":"secret-ish body"}`)})
	if v.ID != 9 || v.State != ytwrite.StateRemoteUnknown || v.Status != ytwrite.StatusPending || v.Reason != ytwrite.ReasonNoEvidence ||
		v.EntityID != "7" || v.YTTargetID != "HK-7" || v.Attempts != 1 || v.Reconciles != 2 || v.ResolvedAt != nil || !v.NextAttemptAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("view = %+v", v)
	}
	done := ytwrite.ViewOf(ytwrite.Op{ID: 10, State: ytwrite.StateApplied, YTResultID: "4-1", ResolvedAt: at})
	if done.Status != ytwrite.StatusApplied || done.YTResultID != "4-1" || done.ResolvedAt == nil || !done.ResolvedAt.Equal(at) {
		t.Fatalf("applied view = %+v", done)
	}
}

// The shared view never carries the raw detail; the operator's view does.
func TestLastErrorOnlyInOperatorView(t *testing.T) {
	op := ytwrite.Op{ID: 9, State: ytwrite.StateRemoteUnknown, LastError: "youtrack answered 500: boom"}
	if v := ytwrite.ViewOf(op); v.LastError != "" {
		t.Fatalf("shared view carries last_error %q", v.LastError)
	}
	if v := ytwrite.OperatorViewOf(op); v.LastError != "youtrack answered 500: boom" || v.ID != 9 {
		t.Fatalf("operator view = %+v", v)
	}
}
