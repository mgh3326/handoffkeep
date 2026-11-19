package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// requireOwnerMismatch asserts err is a fence refusal naming want as the
// record's current owner, and that it matches the sentinel.
func requireOwnerMismatch(t *testing.T, err error, want JobOwner) {
	t.Helper()
	if !errors.Is(err, ErrJobOwnerMismatch) {
		t.Fatalf("want job_owner_mismatch, got %v", err)
	}
	var mm *JobOwnerMismatchError
	if !errors.As(err, &mm) || mm.Owner != want {
		t.Fatalf("current owner: want %+v, got %v", want, err)
	}
}

func TestJobOwnerFenceMatchAndMismatch(t *testing.T) {
	s := schemaStore(t, "job_owner_fence")
	ctx := t.Context()
	jobContextClaim(t, s, "job-fence", nil)

	// An unfenced record reads as no owner.
	read, found, err := s.GetJob(ctx, "job-fence")
	if err != nil || !found || read.OwnerNode != "" || read.OwnerEpoch != 0 {
		t.Fatalf("new record owner: %+v %v %v", read, found, err)
	}

	// Transfer: expected "no owner" and owner node-a at epoch 1, in one update.
	j := jobContextUpdate(t, s, "job-fence", JobUpdate{
		Owner:         &JobOwner{Node: "node-a", Epoch: 1},
		ExpectedOwner: &JobOwner{},
	})
	if j.OwnerNode != "node-a" || j.OwnerEpoch != 1 {
		t.Fatalf("transfer: %+v", j)
	}

	// A stale writer (wrong node) is refused and nothing of its update lands.
	_, err = s.UpdateJob(ctx, "job-fence", JobUpdate{
		Label:         strptr("stale"),
		State:         strptr("spawned"),
		ExpectedOwner: &JobOwner{Node: "node-b", Epoch: 1},
	})
	requireOwnerMismatch(t, err, JobOwner{Node: "node-a", Epoch: 1})
	read, _, _ = s.GetJob(ctx, "job-fence")
	if read.Label != "" || read.State != "claimed" {
		t.Fatalf("refused update applied: label=%q state=%q", read.Label, read.State)
	}

	// The same node with a stale epoch is also refused.
	_, err = s.UpdateJob(ctx, "job-fence", JobUpdate{State: strptr("spawned"), ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 0}})
	requireOwnerMismatch(t, err, JobOwner{Node: "node-a", Epoch: 1})

	// The matching pair applies.
	j = jobContextUpdate(t, s, "job-fence", JobUpdate{State: strptr("spawned"), ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}})
	if j.State != "spawned" {
		t.Fatalf("matching expected owner refused: %+v", j)
	}

	// A reassign must move the epoch forward: the same epoch is refused,
	// a greater one hands the job to node-b.
	_, err = s.UpdateJob(ctx, "job-fence", JobUpdate{Owner: &JobOwner{Node: "node-b", Epoch: 1}})
	requireOwnerMismatch(t, err, JobOwner{Node: "node-a", Epoch: 1})
	j = jobContextUpdate(t, s, "job-fence", JobUpdate{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}})
	if j.OwnerNode != "node-b" || j.OwnerEpoch != 2 {
		t.Fatalf("reassign: %+v", j)
	}
}

func TestJobOwnerFenceNoConditionIsUnfenced(t *testing.T) {
	s := schemaStore(t, "job_owner_fence_compat")
	jobContextClaim(t, s, "job-compat", nil)
	jobContextUpdate(t, s, "job-compat", JobUpdate{Owner: &JobOwner{Node: "node-a", Epoch: 3}})

	// An update without expected_owner behaves exactly as before the fence,
	// whoever the owner is: old panewire nodes keep working.
	j := jobContextUpdate(t, s, "job-compat", JobUpdate{State: strptr("spawned"), Label: strptr("kept")})
	if j.State != "spawned" || j.Label != "kept" || j.OwnerNode != "node-a" || j.OwnerEpoch != 3 {
		t.Fatalf("unconditioned update: %+v", j)
	}
}

func TestJobOwnerFenceTerminalLateWrite(t *testing.T) {
	s := schemaStore(t, "job_owner_fence_terminal")
	ctx := t.Context()
	jobContextClaim(t, s, "job-late-fence", nil)
	jobContextUpdate(t, s, "job-late-fence", JobUpdate{Owner: &JobOwner{Node: "node-a", Epoch: 1}, ExpectedOwner: &JobOwner{}})
	jobContextUpdate(t, s, "job-late-fence", JobUpdate{State: strptr("spawned")})
	jobContextUpdate(t, s, "job-late-fence", JobUpdate{State: strptr("completed")})

	// A stale node's late usage write is refused even on a terminal record.
	usage := &JobUsage{InputTokens: func() *int64 { n := int64(7); return &n }()}
	_, err := s.UpdateJob(ctx, "job-late-fence", JobUpdate{Usage: usage, ExpectedOwner: &JobOwner{Node: "node-b", Epoch: 1}})
	requireOwnerMismatch(t, err, JobOwner{Node: "node-a", Epoch: 1})
	read, _, _ := s.GetJob(ctx, "job-late-fence")
	if read.Usage != nil {
		t.Fatalf("refused late write stored usage: %+v", read.Usage)
	}

	// The owner's own late write lands.
	j := jobContextUpdate(t, s, "job-late-fence", JobUpdate{Usage: usage, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}})
	if j.Usage == nil || *j.Usage.InputTokens != 7 {
		t.Fatalf("owner late write refused: %+v", j.Usage)
	}
}

// A terminal record takes an owner-only transfer when expected_owner matches
// and the epoch moves forward (hk 5303). The transfer changes only the owner:
// state, result and every other field stay as they were.
func TestJobOwnerTransferOnTerminal(t *testing.T) {
	s := schemaStore(t, "job_owner_fence_terminal_transfer")
	ctx := t.Context()

	// lost: the claim ends without a spawn.
	jobContextClaim(t, s, "job-lost-xfer", nil)
	jobContextUpdate(t, s, "job-lost-xfer", JobUpdate{Owner: &JobOwner{Node: "node-a", Epoch: 1}, ExpectedOwner: &JobOwner{}})
	endedAt := time.Now().UTC()
	jobContextUpdate(t, s, "job-lost-xfer", JobUpdate{State: strptr("lost"), At: &endedAt})
	before, _, _ := s.GetJob(ctx, "job-lost-xfer")
	j := jobContextUpdate(t, s, "job-lost-xfer", JobUpdate{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}})
	if j.OwnerNode != "node-b" || j.OwnerEpoch != 2 {
		t.Fatalf("lost transfer: %+v", j)
	}
	// Nil-safe: the terminal timestamps must match whether or not the
	// client supplied one (absent stays absent).
	sameTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.Equal(*b)
	}
	if j.State != "lost" || string(j.Result) != string(before.Result) || j.Label != before.Label ||
		!sameTime(j.EndedAt, before.EndedAt) || !sameTime(j.EndedReceivedAt, before.EndedReceivedAt) {
		t.Fatalf("lost transfer changed the terminal record: before=%+v after=%+v", before, j)
	}

	// completed: the claim spawned and then completed.
	jobContextClaim(t, s, "job-done-xfer", nil)
	jobContextUpdate(t, s, "job-done-xfer", JobUpdate{Owner: &JobOwner{Node: "node-a", Epoch: 1}, ExpectedOwner: &JobOwner{}})
	jobContextUpdate(t, s, "job-done-xfer", JobUpdate{State: strptr("spawned")})
	jobContextUpdate(t, s, "job-done-xfer", JobUpdate{State: strptr("completed")})
	j = jobContextUpdate(t, s, "job-done-xfer", JobUpdate{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}})
	if j.OwnerNode != "node-b" || j.OwnerEpoch != 2 || j.State != "completed" {
		t.Fatalf("completed transfer: %+v", j)
	}
	// The new owner's own late write lands on the transferred record.
	usage := &JobUsage{InputTokens: func() *int64 { n := int64(3); return &n }()}
	j = jobContextUpdate(t, s, "job-done-xfer", JobUpdate{Usage: usage, ExpectedOwner: &JobOwner{Node: "node-b", Epoch: 2}})
	if j.Usage == nil || *j.Usage.InputTokens != 3 {
		t.Fatalf("new owner late write refused: %+v", j.Usage)
	}
}

// An owner write on a terminal record is refused without expected_owner, with
// a stale one, and when combined with any other field (hk 5303).
func TestJobOwnerTransferOnTerminalRefusals(t *testing.T) {
	s := schemaStore(t, "job_owner_fence_terminal_refuse")
	ctx := t.Context()
	jobContextClaim(t, s, "job-refuse", nil)
	jobContextUpdate(t, s, "job-refuse", JobUpdate{Owner: &JobOwner{Node: "node-a", Epoch: 1}, ExpectedOwner: &JobOwner{}})
	jobContextUpdate(t, s, "job-refuse", JobUpdate{State: strptr("lost")})
	before, _, _ := s.GetJob(ctx, "job-refuse")

	// Unfenced: an owner write on a terminal record needs expected_owner.
	_, err := s.UpdateJob(ctx, "job-refuse", JobUpdate{Owner: &JobOwner{Node: "node-b", Epoch: 2}})
	if !errors.Is(err, ErrJobStateConflict) {
		t.Fatalf("unfenced terminal transfer: want job_state_conflict, got %v", err)
	}

	// Stale: the condition names node-b, the record is owned by node-a.
	_, err = s.UpdateJob(ctx, "job-refuse", JobUpdate{Owner: &JobOwner{Node: "node-c", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-b", Epoch: 1}})
	requireOwnerMismatch(t, err, JobOwner{Node: "node-a", Epoch: 1})

	// Combined with a state, a result or a late write: refused, nothing lands.
	combined := []JobUpdate{
		{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}, Label: strptr("moved")},
		{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}, State: strptr("escalated")},
		{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}, State: strptr("completed"), Result: json.RawMessage(`{"ok":true}`)},
		{Owner: &JobOwner{Node: "node-b", Epoch: 2}, ExpectedOwner: &JobOwner{Node: "node-a", Epoch: 1}, Usage: &JobUsage{InputTokens: func() *int64 { n := int64(9); return &n }()}},
	}
	for i, u := range combined {
		if _, err := s.UpdateJob(ctx, "job-refuse", u); !errors.Is(err, ErrJobStateConflict) {
			t.Fatalf("combined %d: want job_state_conflict, got %v", i, err)
		}
	}
	after, _, _ := s.GetJob(ctx, "job-refuse")
	if after.OwnerNode != "node-a" || after.OwnerEpoch != 1 || after.State != "lost" || after.Label != before.Label || after.Usage != nil || string(after.Result) != string(before.Result) {
		t.Fatalf("refused terminal transfer applied: before=%+v after=%+v", before, after)
	}
}
