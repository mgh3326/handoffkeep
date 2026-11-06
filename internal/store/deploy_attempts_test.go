package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestDeployPathMatch needs no database.
func TestDeployPathMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"internal/**", "internal/store/x.go", true},
		{"internal/**", "internal/x.go", true},
		{"internal/**", "web/x.ts", false},
		{"internal/**", "internal", false},
		{"internal/**", "internalx/y.go", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
		{"go.mod", "go.sum", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
		{"docs/*.md", "docs/a.md", true},
		{"[", "[", false},
	} {
		if got := DeployPathMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("DeployPathMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestDeploysMigration(t *testing.T) {
	s := schemaStore(t, "deploys_mig")
	ctx := t.Context()
	for _, table := range []string{"services", "deploys", "deploy_changes"} {
		var found bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, table).Scan(&found); err != nil || !found {
			t.Fatalf("table %s: found=%v err=%v", table, found, err)
		}
	}
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id='20261106-deploy-records')`).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("migration id recorded=%v err=%v", recorded, err)
	}
	insertSvc := `INSERT INTO services(name,repo,path_patterns,deploy_mode,targets,created_at,updated_at) VALUES($1,'example/mig',$2,$3,$4,now(),now())`
	if _, err := s.pool.Exec(ctx, insertSvc, "nopattern", []string{}, "manual", []string{"a"}); err == nil {
		t.Fatal("a service without a pattern was accepted")
	}
	if _, err := s.pool.Exec(ctx, insertSvc, "notarget", []string{"x/**"}, "auto", []string{}); err == nil {
		t.Fatal("a deployed service without a target was accepted")
	}
	if _, err := s.pool.Exec(ctx, insertSvc, "none", []string{"x/**"}, "none", []string{}); err != nil {
		t.Fatalf("deploy_mode none without targets: %v", err)
	}
	insertDeploy := `INSERT INTO deploys(attempt_id,service,targets,action,deployed_ref,state,started_at,finished_at,source,payload_hash) VALUES($1,'none','{a}','deploy','r',$2,now(),$3::timestamptz,'installer','h')`
	if _, err := s.pool.Exec(ctx, insertDeploy, "a1", "running", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, insertDeploy, "a1", "running", nil); err == nil {
		t.Fatal("attempt_id is not unique")
	}
	if _, err := s.pool.Exec(ctx, insertDeploy, "a2", "success", nil); err == nil {
		t.Fatal("a finished state without finished_at was accepted")
	}
}

type deployFixture struct {
	s    *Store
	repo string
	svc  string
}

func newDeployFixture(t *testing.T, prefix string, targets ...string) deployFixture {
	t.Helper()
	s := schemaStore(t, prefix)
	f := deployFixture{s: s, repo: changeRepo(prefix), svc: "svc-" + prefix}
	if _, err := s.PutService(t.Context(), Service{Name: f.svc, Repo: f.repo, PathPatterns: []string{"internal/**", "go.mod"}, DeployMode: DeployModeManual, Targets: targets}); err != nil {
		t.Fatal(err)
	}
	return f
}

// mergedTask is a merged task implementing the given PRs of the fixture repo.
func (f deployFixture) mergedTask(t *testing.T, prs ...int) Task {
	t.Helper()
	x := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	for _, n := range prs {
		send(t, f.s, mergeEvent(x.ID, repoPR(f.repo, n)))
	}
	got, _, err := f.s.GetTask(t.Context(), x.ID)
	if err != nil || got.State != StateMerged {
		t.Fatalf("task %d state %s err %v", x.ID, got.State, err)
	}
	return got
}

func (f deployFixture) start(t *testing.T, attempt, action string, targets []string, changes ...DeployCandidate) Deploy {
	t.Helper()
	d, created, err := f.s.StartDeploy(t.Context(), DeployStart{AttemptID: attempt, Service: f.svc, Targets: targets, Action: action, DeployedRef: "ref-" + attempt, Source: "mergewake", Changes: changes})
	if err != nil || !created {
		t.Fatalf("start %s: created=%v err=%v", attempt, created, err)
	}
	return d
}

func (f deployFixture) finish(t *testing.T, attempt, state string) {
	t.Helper()
	if _, err := f.s.FinishDeploy(t.Context(), attempt, DeployFinish{State: state}); err != nil {
		t.Fatalf("finish %s %s: %v", attempt, state, err)
	}
}

func (f deployFixture) cand(n int, paths ...string) DeployCandidate {
	return DeployCandidate{PR: repoPR(f.repo, n), Paths: paths}
}

func (f deployFixture) contained(t *testing.T, target string, changeID int64) bool {
	t.Helper()
	var ok bool
	if err := f.s.pool.QueryRow(t.Context(), `SELECT `+containedSQL("$1::text", "$2::text", "$3::bigint"), f.svc, target, changeID).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f deployFixture) pendingPRs(t *testing.T) []string {
	t.Helper()
	xs, err := f.s.PendingChanges(t.Context(), f.svc, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, c := range xs {
		out = append(out, c.PR)
	}
	return out
}

func included(d Deploy) map[string]bool {
	out := map[string]bool{}
	for _, c := range d.Changes {
		out[c.PR] = c.Included
	}
	return out
}

func TestStartDeployFreezes(t *testing.T) {
	f := newDeployFixture(t, "deploy_freeze", "prod")
	f.mergedTask(t, 1)
	f.mergedTask(t, 2)
	other := changeRepo("deploy_freeze_other")
	in := DeployStart{AttemptID: "att-1", Service: f.svc, Targets: []string{"prod"}, Action: DeployActionDeploy, DeployedRef: "abc", Source: "installer",
		Changes: []DeployCandidate{f.cand(1, "internal/store/x.go"), f.cand(2, "web/x.ts"), {PR: repoPR(other, 1), Paths: []string{"internal/a.go"}}}}
	d, created, err := f.s.StartDeploy(t.Context(), in)
	if err != nil || !created || d.State != DeployRequested {
		t.Fatalf("start: %+v created=%v err=%v", d, created, err)
	}
	want := map[string]bool{repoPR(f.repo, 1): true, repoPR(f.repo, 2): false, repoPR(other, 1): false}
	if got := included(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("included %v, want %v", got, want)
	}
	for _, c := range d.Changes {
		if c.Included && (c.MatchedPattern == nil || *c.MatchedPattern != "internal/**") {
			t.Fatalf("matched pattern %v", c.MatchedPattern)
		}
	}
	// A PR merged after start is not part of the attempt.
	f.mergedTask(t, 3)
	again, created, err := f.s.StartDeploy(t.Context(), in)
	if err != nil || created || again.ID != d.ID || !reflect.DeepEqual(included(again), want) {
		t.Fatalf("replay: %+v created=%v err=%v", again, created, err)
	}
	changed := in
	changed.DeployedRef = "def"
	if _, _, err = f.s.StartDeploy(t.Context(), changed); !errors.Is(err, ErrDeployAttemptConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	// A PR hk has never seen becomes a merged stub change.
	f.start(t, "att-2", DeployActionDeploy, []string{"prod"}, f.cand(9, "go.mod"))
	if c := mustChange(t, f.s, f.repo, 9); c.State != ChangeMerged || c.MergedAt == nil {
		t.Fatalf("stub change %+v", c)
	}
	if _, _, err = f.s.StartDeploy(t.Context(), DeployStart{AttemptID: "att-3", Service: f.svc, Targets: []string{"staging"}, Action: DeployActionDeploy, DeployedRef: "x", Source: "installer"}); err == nil {
		t.Fatal("a target the service does not have was accepted")
	}
	if _, _, err = f.s.StartDeploy(t.Context(), DeployStart{AttemptID: "att-4", Service: "missing", Targets: []string{"prod"}, Action: DeployActionDeploy, DeployedRef: "x", Source: "installer"}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("unknown service: %v", err)
	}
}

func TestDeployContainedAndPending(t *testing.T) {
	f := newDeployFixture(t, "deploy_contained", "a", "b")
	f.mergedTask(t, 1)
	f.mergedTask(t, 2)
	c := mustChange(t, f.s, f.repo, 1).ID
	if got := f.pendingPRs(t); !reflect.DeepEqual(got, []string{repoPR(f.repo, 1), repoPR(f.repo, 2)}) {
		t.Fatalf("pending before any deploy %v", got)
	}
	f.start(t, "dep-1", DeployActionDeploy, []string{"a", "b"}, f.cand(1, "internal/x.go"), f.cand(2, "web/x.ts"))
	if f.contained(t, "a", c) {
		t.Fatal("contained before the attempt finished")
	}
	f.finish(t, "dep-1", DeploySuccess)
	if !f.contained(t, "a", c) || !f.contained(t, "b", c) {
		t.Fatal("not contained after a successful deploy on a and b")
	}
	// PR 2 was left out by the path filter: not this service's, never pending.
	if got := f.pendingPRs(t); len(got) != 0 {
		t.Fatalf("pending after deploy %v", got)
	}
	snapshot := func() []string {
		rows, err := f.s.pool.Query(t.Context(), `SELECT d.attempt_id||':'||d.state||':'||dc.change_id||':'||dc.included FROM deploys d JOIN deploy_changes dc ON dc.deploy_id=d.id WHERE d.attempt_id='dep-1' ORDER BY dc.change_id`)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	f.start(t, "rb-1", DeployActionRollback, []string{"a"}, f.cand(1, "internal/x.go"))
	f.finish(t, "rb-1", DeploySuccess)
	if f.contained(t, "a", c) || !f.contained(t, "b", c) {
		t.Fatalf("after rollback on a: a=%v b=%v", f.contained(t, "a", c), f.contained(t, "b", c))
	}
	if got := f.pendingPRs(t); !reflect.DeepEqual(got, []string{repoPR(f.repo, 1)}) {
		t.Fatalf("pending after rollback %v", got)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("history changed: %v -> %v", before, after)
	}
	// A failed redeploy does not contain it again; a successful one does.
	f.start(t, "dep-2", DeployActionDeploy, []string{"a"}, f.cand(1, "internal/x.go"))
	f.finish(t, "dep-2", DeployFailed)
	if f.contained(t, "a", c) {
		t.Fatal("a failed attempt contained the change")
	}
	f.start(t, "dep-3", DeployActionDeploy, []string{"a"}, f.cand(1, "internal/x.go"))
	f.finish(t, "dep-3", DeploySuccess)
	if !f.contained(t, "a", c) || len(f.pendingPRs(t)) != 0 {
		t.Fatalf("redeploy: contained=%v pending=%v", f.contained(t, "a", c), f.pendingPRs(t))
	}
	if _, err := f.s.PendingChanges(t.Context(), "missing", 10); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("unknown service: %v", err)
	}
}

func TestFinishDeployMovesTask(t *testing.T) {
	f := newDeployFixture(t, "deploy_task", "a", "b")
	x := f.mergedTask(t, 1, 2)
	bare := flowTask(t, f.s, f.s.pool, "implement", StateMerged, false)
	state := func(id int64) string {
		t.Helper()
		got, _, err := f.s.GetTask(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	f.start(t, "t-1", DeployActionDeploy, []string{"a", "b"}, f.cand(1, "internal/x.go"))
	f.finish(t, "t-1", DeploySuccess)
	if got := state(x.ID); got != StateMerged {
		t.Fatalf("one of two changes deployed: %s", got)
	}
	f.start(t, "t-2", DeployActionDeploy, []string{"a"}, f.cand(2, "internal/y.go"))
	f.finish(t, "t-2", DeploySuccess)
	if got := state(x.ID); got != StateMerged {
		t.Fatalf("second change on a only: %s", got)
	}
	f.start(t, "t-3", DeployActionDeploy, []string{"b"}, f.cand(2, "internal/y.go"))
	f.finish(t, "t-3", DeployFailed)
	if got := state(x.ID); got != StateMerged {
		t.Fatalf("a failed finish moved the task: %s", got)
	}
	f.start(t, "t-4", DeployActionDeploy, []string{"b"}, f.cand(2, "internal/y.go"))
	f.finish(t, "t-4", DeploySuccess)
	if got := state(x.ID); got != StateDeployed {
		t.Fatalf("every change on every target: %s", got)
	}
	if ev := lastEvent(t, f.s, x.ID); ev.Source != SourceDeploy || ev.SourceRef != "deploy:t-4" {
		t.Fatalf("event %+v", ev)
	}
	if got := state(bare.ID); got != StateMerged {
		t.Fatalf("a task with no change moved: %s", got)
	}
	f.finish(t, "t-4", DeploySuccess)
	if _, err := f.s.FinishDeploy(t.Context(), "t-4", DeployFinish{State: DeployFailed}); !errors.Is(err, ErrDeployAttemptFinished) {
		t.Fatalf("finish twice with another state: %v", err)
	}
	if _, err := f.s.FinishDeploy(t.Context(), "t-missing", DeployFinish{State: DeploySuccess}); !errors.Is(err, ErrDeployNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
	// A change of a repo without a deployed service needs no deploy: a task
	// whose other change is contained moves.
	y := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	send(t, f.s, mergeEvent(y.ID, repoPR(f.repo, 5)))
	send(t, f.s, mergeEvent(y.ID, repoPR(changeRepo("deploy_task_noservice"), 1)))
	f.start(t, "t-5", DeployActionDeploy, []string{"a", "b"}, f.cand(5, "go.mod"))
	f.finish(t, "t-5", DeploySuccess)
	if got := state(y.ID); got != StateDeployed {
		t.Fatalf("no-service change blocked the task: %s", got)
	}
	// Revert and reland: the reverted PR, never deployed, does not block the
	// reland's deploy.
	z := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	send(t, f.s, mergeEvent(z.ID, repoPR(f.repo, 6)))
	send(t, f.s, revertEvent(z.ID, repoPR(f.repo, 6), repoPR(f.repo, 7)))
	send(t, f.s, mergeEvent(z.ID, repoPR(f.repo, 8)))
	if got := state(z.ID); got != StateMerged {
		t.Fatalf("reland: %s", got)
	}
	f.start(t, "t-6", DeployActionDeploy, []string{"a", "b"}, f.cand(8, "internal/z.go"))
	f.finish(t, "t-6", DeploySuccess)
	if got := state(z.ID); got != StateDeployed {
		t.Fatalf("a reverted change blocked the reland: %s", got)
	}
}
