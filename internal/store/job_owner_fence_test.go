package store

import (
	"errors"
	"testing"
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
