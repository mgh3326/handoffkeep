package store

import (
	"errors"
	"path"
	"strings"
	"testing"
	"time"
)

// The attempt writers (StartDeploy, FinishDeploy) were removed in hk 5913. The
// containment readers (SettleMerged, changeDeployedSQL) still read the services,
// deploys and deploy_changes rows, so these fixtures write those rows directly,
// the way the removed writers did.

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

// deployCandidate is one PR a fixture attempt names, with the paths it changed.
type deployCandidate struct {
	PR    string
	Paths []string
}

type deployFixture struct {
	s        *Store
	repo     string
	svc      string
	patterns []string
}

// newDeployFixture registers one manual service on the fixture repo, with path
// patterns internal/** and go.mod, and the given targets.
func newDeployFixture(t *testing.T, prefix string, targets ...string) deployFixture {
	t.Helper()
	s := schemaStore(t, prefix)
	f := deployFixture{s: s, repo: changeRepo(prefix), svc: "svc-" + prefix, patterns: []string{"internal/**", "go.mod"}}
	if targets == nil {
		targets = []string{}
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO services(name,repo,path_patterns,deploy_mode,targets,created_at,updated_at) VALUES($1,$2,$3,'manual',$4,now(),now())`, f.svc, f.repo, f.patterns, targets); err != nil {
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

// start writes one attempt in state requested with its frozen changes: a PR of
// the fixture repo is included when one of its paths matches a pattern, and a
// PR hk has not seen becomes a merged stub change.
func (f deployFixture) start(t *testing.T, attempt, action string, targets []string, changes ...deployCandidate) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO deploys(attempt_id,service,targets,action,deployed_ref,state,started_at,source,payload_hash) VALUES($1,$2,$3,$4,$5,'requested',$6,'mergewake','fixture') RETURNING id`, attempt, f.svc, targets, action, "ref-"+attempt, now).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		repo, n, _ := parsePRURL(c.PR)
		var changeID int64
		err := tx.QueryRow(ctx, `SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, repo, n).Scan(&changeID)
		if errors.Is(err, ErrNoRows) {
			changeID, err = upsertChangeTx(ctx, tx, changeWrite{repo: repo, number: n, state: ChangeMerged, mergedAt: &now}, now, now)
		}
		if err != nil {
			t.Fatal(err)
		}
		var matched *string
		if repo == f.repo {
			matched = f.matchedPattern(c.Paths)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO deploy_changes(deploy_id,change_id,included,matched_pattern) VALUES($1,$2,$3,$4)`, id, changeID, matched != nil, matched); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// finish moves an attempt to its final state and stamps finished_at, which the
// containment rule orders by.
func (f deployFixture) finish(t *testing.T, attempt, state string) {
	t.Helper()
	if _, err := f.s.pool.Exec(t.Context(), `UPDATE deploys SET state=$2, finished_at=clock_timestamp() WHERE attempt_id=$1`, attempt, state); err != nil {
		t.Fatalf("finish %s %s: %v", attempt, state, err)
	}
}

func (f deployFixture) cand(n int, paths ...string) deployCandidate {
	return deployCandidate{PR: repoPR(f.repo, n), Paths: paths}
}

// matchedPattern returns the first fixture pattern that one of paths matches, or nil.
func (f deployFixture) matchedPattern(paths []string) *string {
	for _, pat := range f.patterns {
		for _, p := range paths {
			if deployPathMatch(pat, p) {
				return &pat
			}
		}
	}
	return nil
}

// deployPathMatch: "dir/**" matches every path under dir/, any other pattern is path.Match.
func deployPathMatch(pattern, p string) bool {
	if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(p, dir+"/")
	}
	ok, err := path.Match(pattern, p)
	return err == nil && ok
}
