package fleetmetrics

import (
	"os"
	"testing"
	"time"
)

// Real-shape fixtures: testdata/jobs/627-… is a verbatim copy of a job
// directory (events + completion-sentinel.log), reps-list.txt is verbatim
// `scopefuel reps list --limit 3` output, deploy-*.{json,yaml} are verbatim
// deploy-record bodies from hk.

func TestParseJobDirRealShape(t *testing.T) {
	j, err := ParseJobDir("testdata/jobs/627-relay-lost-revoked-20260924-1540", "mac-personal")
	if err != nil {
		t.Fatal(err)
	}
	if j.Role != "builder" || j.OwnerLane != "b627-relay-lost-revoked" || j.ParentLane != "director-1" || j.Tier != "T2" || j.Profile != "builder-sol" || j.PaneID != "w16:p2JF" {
		t.Fatalf("job header = %+v", j)
	}
	kinds := []string{"job.claim", "quota_pool.record", "job.spawned", "job.completed", "job.reaped", "job.lost"}
	if len(j.Events) != len(kinds) {
		t.Fatalf("events = %d, want %d", len(j.Events), len(kinds))
	}
	for i, k := range kinds {
		if j.Events[i].Kind != k || j.Events[i].At == nil {
			t.Fatalf("event %d = %+v, want kind %s with a time", i, j.Events[i], k)
		}
	}
	if j.Events[0].AtSource != "payload" || !j.Events[0].At.Equal(time.Date(2026, 9, 24, 6, 36, 50, 0, time.UTC)) {
		t.Fatalf("claim time = %v (%s)", j.Events[0].At, j.Events[0].AtSource)
	}
	// Flat completion records carry no timestamp: the file mtime is used and
	// labelled as such; job.reaped carries "at".
	if j.Events[3].AtSource != "mtime" || j.Events[4].AtSource != "payload" || j.Events[5].AtSource != "mtime" {
		t.Fatalf("time sources = %s/%s/%s", j.Events[3].AtSource, j.Events[4].AtSource, j.Events[5].AtSource)
	}
	if j.Events[5].Reason != "agent_not_found" || j.Events[3].Epoch != 1 {
		t.Fatalf("lost reason %q epoch %d", j.Events[5].Reason, j.Events[3].Epoch)
	}
	if len(j.Status) == 0 || j.Status[0].Status != "working" || !j.Status[0].From.Equal(time.Date(2026, 9, 24, 6, 37, 29, 0, time.UTC)) {
		t.Fatalf("first status run = %+v", j.Status)
	}
	var gone *StatusRun
	for i := range j.Status {
		if j.Status[i].Status == "gone" {
			gone = &j.Status[i]
			break
		}
	}
	if gone == nil || !gone.From.Equal(time.Date(2026, 9, 24, 8, 2, 57, 0, time.UTC)) {
		t.Fatalf("first gone run = %+v", gone)
	}
}

func TestParseSentinelLogRuns(t *testing.T) {
	runs := ParseSentinelLog("2026-09-24T00:00:00Z status=working transient=0 action=none\n" +
		"2026-09-24T00:00:30Z status=working transient=0 action=none\n" +
		"2026-09-24T00:01:00Z status=done transient=0 action=completed\n" +
		"2026-09-24T00:01:30Z status=blocked transient=0 action=none\n" +
		"wrk: warning: job.completed already recorded\n" +
		"2026-09-24T00:10:00Z status=err:parse transient=1 action=none\n")
	want := []StatusRun{
		{From: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 24, 0, 1, 0, 0, time.UTC), Status: "working"},
		{From: time.Date(2026, 9, 24, 0, 1, 0, 0, time.UTC), To: time.Date(2026, 9, 24, 0, 1, 30, 0, time.UTC), Status: "idle"},
		{From: time.Date(2026, 9, 24, 0, 10, 0, 0, time.UTC), To: time.Date(2026, 9, 24, 0, 10, 0, 0, time.UTC), Status: "unknown"},
	}
	if len(runs) != len(want) {
		t.Fatalf("runs = %+v", runs)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Fatalf("run %d = %+v, want %+v", i, runs[i], want[i])
		}
	}
	// The 8.5-minute silence is not observed: nothing covers 00:05.
	if st := statusAt(runs, time.Date(2026, 9, 24, 0, 5, 0, 0, time.UTC)); st != "" {
		t.Fatalf("status in gap = %q, want unobserved", st)
	}
}

func TestParseRepsListRealShape(t *testing.T) {
	b, err := os.ReadFile("testdata/reps-list.txt")
	if err != nil {
		t.Fatal(err)
	}
	reps := ParseRepsList(string(b), "fixture")
	if len(reps) != 3 {
		t.Fatalf("reps = %d, want 3", len(reps))
	}
	var impl *Rep
	for i := range reps {
		if reps[i].Role == "impl" {
			impl = &reps[i]
		}
	}
	if impl == nil || impl.Task != "646" || impl.Rounds == nil || *impl.Rounds != 3 || *impl.BlockersFound != 4 || impl.Profile != "builder-opus" || impl.RecordedAt.IsZero() {
		t.Fatalf("impl rep = %+v", impl)
	}
}

func TestParseDeployDocs(t *testing.T) {
	b, _ := os.ReadFile("testdata/deploy-handoffkeep.json")
	d := ParseDeployDoc("deploy/handoffkeep/20260924T070958Z", string(b))
	if d.Format != "json" || d.Service != "handoffkeep" || d.Result != "success" || d.DeployedRef != "dbbcc7b92384a19769fbe65f2cb66f2020320726" || len(d.IncludedPRs) != 3 || d.DeployedAt == nil || !d.DeployedAt.Equal(time.Date(2026, 9, 24, 7, 9, 58, 0, time.UTC)) {
		t.Fatalf("json record = %+v", d)
	}
	b, _ = os.ReadFile("testdata/deploy-scopefuel.yaml")
	d = ParseDeployDoc("deploy/scopefuel/20260924T080817Z", string(b))
	// The YAML record has no result field: unknown, never "success".
	if d.Format != "yaml" || d.Service != "scopefuel" || d.Result != "" || d.DeployedRef != "40d901c10c32380a99ac2a8a409b62d34e567df7" || d.DeployedAt == nil {
		t.Fatalf("yaml record = %+v", d)
	}
	if d := ParseDeployDoc("deploy/x/y", "not a record: [unbalanced"); d.Format != "invalid" || d.Result != "" {
		t.Fatalf("invalid record = %+v", d)
	}
}

func TestParseHostsSlots(t *testing.T) {
	caps, err := ParseHostsSlots([]byte("[local]\nmax_active = 6\n\n[hosts.desktop]\nssh = \"desktop\"\ncapacity = 5\n\n[hosts.oci]\nssh = \"oci\"\n"), "mac-personal", "hosts.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 2 || caps[0].Machine != "desktop" || caps[0].Slots != 5 || caps[1].Machine != "mac-personal" || caps[1].Slots != 6 {
		t.Fatalf("caps = %+v (oci has no capacity and is not a slot host)", caps)
	}
}
