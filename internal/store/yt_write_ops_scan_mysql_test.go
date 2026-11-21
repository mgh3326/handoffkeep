//go:build mysql

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// TestMySQLOpenOpReadsUseStateIndexes seeds yt_write_ops at production shape
// (about 47,000 resolved ops with old timestamps, and a few open ones), then
// checks each MySQL statement that reads open ops (hk 5878): the outer
// yt_write_ops read of its plan uses an index other than PRIMARY, and the plan
// estimates under 1,000 rows. The digests showed each of these examining the
// whole table per call. The same reads also return the open ops the seed made.
func TestMySQLOpenOpReadsUseStateIndexes(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	const tenant = "plan-tenant"
	seedResolvedOpsForPlan(t, ctx, s, old, tenant, 47000)
	open := seedOpenOpsForPlan(t, ctx, s, old, tenant)

	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)

	// The dispatcher's reads, as Drive runs them.
	explainOpenRead(t, ctx, s, "stale", ytStaleSQL(s), now.UTC(), 100)
	explainOpenRead(t, ctx, s, "due", ytDueMySQLSQL(s), ytDueMySQLArgs(now, tenant, 100)...)

	// The alert scans, as the alert tick runs them: each arm is its own read.
	for _, class := range []string{ytWriteOpAlertStuck, ytWriteOpAlertRejected} {
		arms, err := ytWriteOpAlertArms(tx, class, now)
		if err != nil {
			t.Fatal(err)
		}
		for i, arm := range arms {
			explainOpenRead(t, ctx, s, fmt.Sprintf("alert %s arm %d", class, i), arm.sql, arm.args...)
		}
	}

	// The same reads return the open ops the seed made, and no resolved one.
	stale, err := s.YTWriteLedger().Stale(ctx, now.UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != open.dispatched {
		t.Fatalf("stale = %+v, want only op %d", stale, open.dispatched)
	}
	due, err := s.YTTenantLedger(tenant).Due(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsOpID(due, open.remoteUnknown) {
		t.Fatalf("due = %+v, want remote_unknown op %d", due, open.remoteUnknown)
	}
	for _, op := range due {
		if op.State != ytwrite.StatePrepared && op.State != ytwrite.StateRemoteUnknown {
			t.Fatalf("due lists a %s op %d", op.State, op.ID)
		}
	}
	stuck, err := s.ytWriteOpAlertCandidates(ctx, tx, ytWriteOpAlertStuck, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 3 {
		t.Fatalf("stuck candidates = %d, want 3 (prepared, dispatched, remote_unknown)", len(stuck))
	}
	rejected, err := s.ytWriteOpAlertCandidates(ctx, tx, ytWriteOpAlertRejected, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected candidates = %d, want 2", len(rejected))
	}
}

// explainOpenRead checks the outer yt_write_ops read in one statement's plan.
// The outer table is aliased o in the arms that read it and named in full
// where it is not aliased; subqueries use other aliases and are not checked.
func explainOpenRead(t *testing.T, ctx context.Context, s *Store, name, query string, args ...any) {
	t.Helper()
	rows, err := s.db().Query(ctx, "EXPLAIN FORMAT=TRADITIONAL "+query, args...)
	if err != nil {
		t.Fatalf("%s: EXPLAIN: %v", name, err)
	}
	defer rows.Close()
	outer := 0
	for rows.Next() {
		var id, selectType, table, partitions, typ, possible, key, keyLen, ref, est, filtered, extra sql.NullString
		if err := rows.Scan(&id, &selectType, &table, &partitions, &typ, &possible, &key, &keyLen, &ref, &est, &filtered, &extra); err != nil {
			t.Fatalf("%s: scan plan: %v", name, err)
		}
		if table.String != "o" && table.String != "yt_write_ops" {
			continue
		}
		outer++
		if !key.Valid || key.String == "PRIMARY" {
			t.Errorf("%s: outer read uses key %q (type %s, extra %q); want a state index", name, key.String, typ.String, extra.String)
		}
		if n, err := strconv.Atoi(est.String); err != nil || n >= 1000 {
			t.Errorf("%s: plan estimates %q rows; want under 1000", name, est.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: plan rows: %v", name, err)
	}
	if outer == 0 {
		t.Errorf("%s: plan has no read of yt_write_ops", name)
	}
}

// yt_write_ops columns the seed writes, in order (every NOT NULL column).
const opSeedCols = `(principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id, yt_target_id, actor, child_seq, state, yt_result_id, reason, last_error, attempts, reconciles, created_at, dispatched_at, next_attempt_at, resolved_at, tenant)`

// opSeedRow is one op's values in opSeedCols order.
type opSeedRow struct {
	key, state, result, reason string
	created, dispatched, next  time.Time
	resolved                   bool
	tenant                     string
}

func (r opSeedRow) args() []any {
	var dispatched, resolved any
	if !r.dispatched.IsZero() {
		dispatched = r.dispatched
	}
	if r.resolved {
		resolved = r.created
	}
	return []any{"plan", "comment_append", r.key, "sha", "{}", "task", r.key, "", "plan", 0,
		r.state, r.result, r.reason, "", 0, 0, r.created, dispatched, r.next, resolved, r.tenant}
}

// seedResolvedOpsForPlan inserts n applied ops whose timestamps are old enough
// that a "<= now" bound covers all of them, as production's resolved ops are.
func seedResolvedOpsForPlan(t *testing.T, ctx context.Context, s *Store, at time.Time, tenant string, n int) {
	t.Helper()
	const batch = 1000
	const row = `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var args []any
		for i := start; i < end; i++ {
			args = append(args, opSeedRow{key: fmt.Sprintf("seed-%d", i), state: "applied", result: "y",
				created: at, dispatched: at, next: at, resolved: true, tenant: tenant}.args()...)
		}
		q := `INSERT INTO yt_write_ops ` + opSeedCols + ` VALUES ` + strings.TrimSuffix(strings.Repeat(row+",", end-start), ",")
		if _, err := s.db().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// openPlanOps are the ids of the seeded ops the checks look for.
type openPlanOps struct{ dispatched, remoteUnknown int64 }

// seedOpenOpsForPlan inserts one op in each open state, plus two rejected ops,
// all due or stale against now, and returns the ids of two of them.
func seedOpenOpsForPlan(t *testing.T, ctx context.Context, s *Store, at time.Time, tenant string) openPlanOps {
	t.Helper()
	rows := []opSeedRow{
		{key: "open-dispatched", state: "dispatched", created: at, dispatched: at, next: at, tenant: tenant},
		{key: "open-remote-unknown", state: "remote_unknown", created: at, next: at, tenant: tenant},
		{key: "open-prepared", state: "prepared", created: at, next: at, tenant: tenant},
		{key: "open-rejected-a", state: "rejected", result: "y", reason: "ambiguous_issue", created: at, next: at, resolved: true, tenant: tenant},
		{key: "open-rejected-b", state: "rejected", result: "y", reason: "ambiguous_issue", created: at, next: at, resolved: true, tenant: tenant},
	}
	for _, r := range rows {
		if _, err := s.db().Exec(ctx, `INSERT INTO yt_write_ops `+opSeedCols+` VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.args()...); err != nil {
			t.Fatal(err)
		}
	}
	var out openPlanOps
	idOf := func(key string) int64 {
		var id int64
		if err := s.db().QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE principal='plan' AND kind='comment_append' AND idem_key=?`, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	out.dispatched = idOf("open-dispatched")
	out.remoteUnknown = idOf("open-remote-unknown")
	return out
}

func containsOpID(ops []ytwrite.Op, id int64) bool {
	for _, op := range ops {
		if op.ID == id {
			return true
		}
	}
	return false
}
