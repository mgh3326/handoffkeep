package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

// ConfigureWrk publishes only a complete validated snapshot. An off server
// retains durable legacy fences, but cannot enroll or append O3 records.
func (s *Store) ConfigureWrk(ctx context.Context, mode string, policy *wrkrecords.Policy, generation string, recovery bool) error {
	return s.ConfigureWrkEngine(ctx, wrkrecords.Engine{Policy: policy, Mode: mode, Recovery: recovery, Generation: generation})
}

// ConfigureWrkEngine publishes a complete immutable configuration, with an
// optional fixed clock for deterministic contract execution. Runtime leaves
// Now zero and uses the server UTC clock.
func (s *Store) ConfigureWrkEngine(ctx context.Context, engine wrkrecords.Engine) error {
	mode, policy, generation := engine.Mode, engine.Policy, engine.Generation
	if mode == "" {
		mode = "off"
	}
	if mode != "off" && mode != "shadow" && mode != "enforce" {
		return wrkrecords.Error("wrk_unavailable")
	}
	installed, err := s.validateWrkRuntime(ctx)
	if err != nil {
		return err
	}
	if mode != "off" && (!installed || policy == nil || !wrkrecords.UUIDRE.MatchString(generation)) {
		return wrkrecords.Error("wrk_unavailable")
	}
	engine.Mode = mode
	if !installed {
		s.wrk.Store(&engine)
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrkrecords.Error("wrk_unavailable")
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return wrkrecords.Error("wrk_unavailable")
	}
	snapshot, err := loadWrkSnapshot(ctx, tx)
	if err != nil {
		return wrkrecords.Error("wrk_unavailable")
	}
	if policy != nil {
		if err = policy.ValidateGraph(snapshot, s.WrkEngine().Policy); err != nil {
			return err
		}
	}
	// Keep the graph lock through atomic publication. Failed replacements never
	// replace the previously validated engine.
	s.wrk.Store(&engine)
	return tx.Commit(ctx)
}
func (s *Store) validateWrkRuntime(ctx context.Context) (bool, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM unnest(ARRAY[to_regclass('wrk_records'),to_regclass('wrk_contents'),to_regclass('wrk_attempts'),to_regclass('wrk_job_locks')]) x WHERE x IS NOT NULL`).Scan(&count)
	if err != nil {
		return false, wrkrecords.Error("wrk_unavailable")
	}
	if count == 0 {
		return false, nil
	}
	if count != 4 {
		return true, wrkrecords.Error("wrk_unavailable")
	}
	var safe bool
	err = s.pool.QueryRow(ctx, `SELECT bool_and(NOT r.rolsuper AND NOT r.rolbypassrls AND NOT pg_has_role(current_user,c.relowner,'MEMBER') AND NOT has_table_privilege(current_user,c.oid,'UPDATE') AND NOT has_table_privilege(current_user,c.oid,'DELETE') AND NOT has_table_privilege(current_user,c.oid,'TRUNCATE')) FROM pg_class c CROSS JOIN pg_roles r WHERE r.rolname=current_user AND c.oid IN ('wrk_records'::regclass,'wrk_contents'::regclass,'wrk_attempts'::regclass)`).Scan(&safe)
	if err != nil || !safe {
		return true, wrkrecords.Error("wrk_unavailable")
	}
	return true, nil
}

func (s *Store) WrkEngine() wrkrecords.Engine {
	e := s.wrk.Load()
	if e == nil {
		return wrkrecords.Engine{Mode: "off"}
	}
	return *e
}

type wrkQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadWrkSnapshot(ctx context.Context, q wrkQuerier) (wrkrecords.Snapshot, error) {
	s := wrkrecords.Snapshot{Jobs: map[string][]wrkrecords.Record{}, Contents: map[string]map[int64][]byte{}}
	var installed bool
	if e := q.QueryRow(ctx, `SELECT to_regclass('wrk_records') IS NOT NULL`).Scan(&installed); e != nil {
		return s, e
	}
	if !installed {
		return s, nil
	}
	rows, e := q.Query(ctx, `SELECT job_id,seq,record_id::text,store_generation::text,kind,claim_seq,actor_client_id,idempotency_key::text,request_sha256,payload,imported,source,received_at,previous_hash,record_hash FROM wrk_records ORDER BY job_id,seq`)
	if e != nil {
		return s, e
	}
	for rows.Next() {
		var r wrkrecords.Record
		var payload, source []byte
		var at time.Time
		if e = rows.Scan(&r.JobID, &r.Seq, &r.RecordID, &r.Generation, &r.Kind, &r.ClaimSeq, &r.Actor, &r.Key, &r.RequestSHA, &payload, &r.Imported, &source, &at, &r.PreviousHash, &r.RecordHash); e != nil {
			rows.Close()
			return s, e
		}
		if e = json.Unmarshal(payload, &r.Payload); e != nil {
			rows.Close()
			return s, e
		}
		if len(source) > 0 {
			if e = json.Unmarshal(source, &r.Source); e != nil {
				rows.Close()
				return s, e
			}
		}
		r.ReceivedAt = at.UTC().Format("2006-01-02T15:04:05.000000Z")
		s.Jobs[r.JobID] = append(s.Jobs[r.JobID], r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return s, e
	}
	rows, e = q.Query(ctx, `SELECT job_id,seq,content FROM wrk_contents ORDER BY job_id,seq`)
	if e != nil {
		return s, e
	}
	defer rows.Close()
	for rows.Next() {
		var job string
		var seq int64
		var b []byte
		if e = rows.Scan(&job, &seq, &b); e != nil {
			return s, e
		}
		if s.Contents[job] == nil {
			s.Contents[job] = map[int64][]byte{}
		}
		s.Contents[job][seq] = b
	}
	return s, rows.Err()
}

// WrkAttempt deliberately stores only the closed audit metadata vocabulary.
func (s *Store) WrkAttempt(ctx context.Context, actor, job, kind string, err error) {
	var a, j, k any
	if actor != "" && wrkrecords.JobRE.MatchString(actor) {
		a = actor
	}
	if wrkrecords.JobRE.MatchString(job) {
		j = job
	}
	if wrkrecords.Validate("Input_"+kind, wrkrecords.Object{}) != nil { // Kind is audited only when it is a known request kind.
		for _, known := range []string{"claim", "activate", "round_open", "verdict", "void", "joined", "completed", "grant", "grant_cancel", "end", "import", "import_begin", "import_seal"} {
			if kind == known {
				k = kind
			}
		}
	}
	_, _ = s.pool.Exec(ctx, `INSERT INTO wrk_attempts(attempt_id,actor_client_id,job_id,kind,stable_error_code,received_at) VALUES($1,$2,$3,$4,$5,now())`, wrkrecords.NewUUID(), a, j, k, wrkrecords.Code(err))
}
func (s *Store) WrkAppend(ctx context.Context, actor, job, key string, r wrkrecords.Request) (out wrkrecords.WriteResponse, err error) {
	defer func() {
		if err != nil {
			s.WrkAttempt(ctx, actor, job, r.Kind, err)
		}
	}()
	engine := s.WrkEngine()
	if err = engine.Check(actor, wrkPermission(r.Kind, engine.Policy, actor, r.Payload)); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, wrkrecords.Error("wrk_unavailable")
	}
	defer tx.Rollback(ctx)
	if r.Kind == "claim" || r.Kind == "activate" {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
			return out, err
		}
	}
	// A policy publication may have completed while this append acquired locks.
	engine = s.WrkEngine()
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return out, err
	}
	snapshot, err := loadWrkSnapshot(ctx, tx)
	if err != nil {
		return out, err
	}
	// All referenced jobs, including predecessor/reviewer/subject, are locked in
	// one lexical order. Reading the planning snapshot never authorizes a write.
	jobs := map[string]bool{job: true}
	for id := range snapshot.Jobs {
		jobs[id] = true
	}
	if review := wrkrecords.Obj(r.Payload, "review_of"); review != nil {
		jobs[wrkrecords.Text(review, "job_id")] = true
	}
	for _, field := range []string{"predecessor_job_id", "tester_job_id"} {
		if id := wrkrecords.Text(r.Payload, field); id != "" {
			jobs[id] = true
		}
	}
	ordered := make([]string, 0, len(jobs))
	for id := range jobs {
		if !wrkrecords.JobRE.MatchString(id) {
			return out, wrkrecords.Error("invalid_request")
		}
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if _, err = tx.Exec(ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
			return out, err
		}
		var locked string
		if err = tx.QueryRow(ctx, `SELECT job_id FROM wrk_job_locks WHERE job_id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			return out, err
		}
	}
	snapshot, err = loadWrkSnapshot(ctx, tx)
	if err != nil {
		return out, err
	}
	var content []byte
	out, content, err = engine.Append(snapshot, actor, job, key, r)
	if err != nil {
		return out, err
	}
	if !out.Created {
		return out, tx.Commit(ctx)
	}
	rec := out.Record
	if rec.Kind == "claim" && !wrkrecords.Review(&rec) {
		id := wrkrecords.Num(rec.Payload, "task_id")
		var task Task
		err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &task)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, wrkrecords.Error("not_found")
		}
		if err != nil {
			return out, err
		}
		expectedOld := wrkrecords.Text(rec.Payload, "predecessor_job_id")
		if expectedOld == "" && r.Payload["supersedes"] != nil {
			expectedOld = job
		}
		if task.Refs.JobID != "" && task.Refs.JobID != job && task.Refs.JobID != expectedOld {
			return out, wrkrecords.Error("claim_conflict")
		}
		// Claim corrections cannot detach a protected task outside this transaction.
		if prior := snapshot.Claim(job); prior != nil && wrkrecords.Num(prior.Payload, "task_id") != id {
			oldID := wrkrecords.Num(prior.Payload, "task_id")
			if _, err = tx.Exec(ctx, `UPDATE tasks SET refs=refs-'job_id',claimed_by='',updated_at=now() WHERE id=$1 AND refs->>'job_id'=$2`, oldID, job); err != nil {
				return out, err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs,'{job_id}',to_jsonb($2::text)),claimed_by=$3,updated_at=now() WHERE id=$1`, id, job, wrkrecords.Text(rec.Payload, "owner_lane")); err != nil {
			return out, err
		}
	}
	payload, _ := json.Marshal(rec.Payload)
	var source any
	if rec.Source != nil {
		b, _ := json.Marshal(rec.Source)
		source = string(b)
	}
	_, err = tx.Exec(ctx, `INSERT INTO wrk_records(job_id,seq,record_id,store_generation,kind,claim_seq,actor_client_id,idempotency_key,request_sha256,payload,imported,source,received_at,previous_hash,record_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12::jsonb,$13,$14,$15)`, rec.JobID, rec.Seq, rec.RecordID, rec.Generation, rec.Kind, rec.ClaimSeq, rec.Actor, rec.Key, rec.RequestSHA, string(payload), rec.Imported, source, rec.ReceivedAt, rec.PreviousHash, rec.RecordHash)
	if err != nil {
		return out, err
	}
	if len(content) > 0 {
		if _, err = tx.Exec(ctx, `INSERT INTO wrk_contents(job_id,seq,content,content_sha256,size_bytes) VALUES($1,$2,$3,$4,$5)`, job, rec.Seq, content, wrkrecords.SHA(content), len(content)); err != nil {
			return out, err
		}
	}
	if (rec.Kind == "completed" || rec.Kind == "joined") && (snapshot.ReceiptRequired(job) || out.Summary.State == "active") {
		claim := snapshot.Claim(job)
		if claim == nil {
			return out, wrkrecords.Error("claim_conflict")
		}
		// Canonical projection has a deterministic identity and no caller path.
		_, err = tx.Exec(ctx, `INSERT INTO relay_events(kind,job_id,epoch,owner_lane,report_path,reason,head,pr,received_at) VALUES($1,$2,0,$3,$4,'',$5,$6,now()) ON CONFLICT DO NOTHING`, "job."+rec.Kind, job, wrkrecords.Text(claim.Payload, "owner_lane"), "wrk:"+job+":"+intString(rec.Seq), wrkrecords.Text(rec.Payload, "head"), wrkrecords.Text(rec.Payload, "pr"))
		if err != nil {
			return out, err
		}
	}
	err = tx.Commit(ctx)
	return out, err
}
func wrkPermission(kind string, p *wrkrecords.Policy, actor string, payload wrkrecords.Object) string {
	if kind == "claim" {
		if role := wrkrecords.Text(payload, "role"); role == "tester" || role == "reviewer" || role == "counsel" {
			return "reviewer.claim.write"
		}
		return "claim.write"
	}
	return map[string]string{"activate": "activate.write", "round_open": "round.open", "verdict": "verdict.write", "void": "void.write", "joined": "joined.write", "completed": "completed.write", "grant": "grant.write", "grant_cancel": "grant.cancel", "end": "end.write", "import_begin": "import.begin", "import_seal": "import.seal", "import": "import.write"}[kind]
}
func intString(n int64) string { b, _ := json.Marshal(n); return string(b) }
func (s *Store) WrkSnapshot(ctx context.Context, actor, permission string) (wrkrecords.Snapshot, error) {
	if err := s.WrkEngine().Check(actor, permission); err != nil {
		return wrkrecords.Snapshot{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return wrkrecords.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	snap, err := loadWrkSnapshot(ctx, tx)
	if err != nil {
		return snap, err
	}
	return snap, tx.Commit(ctx)
}
func (s *Store) WrkJob(ctx context.Context, actor, job string) (wrkrecords.JobResponse, error) {
	snap, e := s.WrkSnapshot(ctx, actor, "job.read")
	if e != nil {
		return wrkrecords.JobResponse{}, e
	}
	c := snap.Claim(job)
	if c == nil {
		return wrkrecords.JobResponse{}, wrkrecords.Error("not_found")
	}
	sum, e := snap.Summary(job, s.WrkEngine().Time())
	return wrkrecords.JobResponse{Contract: wrkrecords.Contract, Claim: *c, Summary: sum}, e
}
func (s *Store) WrkRecords(ctx context.Context, actor, job string, after, snapshot int64, limit int) (wrkrecords.RecordsResponse, error) {
	out := wrkrecords.RecordsResponse{Contract: wrkrecords.Contract, Records: []wrkrecords.Record{}, Next: after}
	snap, e := s.WrkSnapshot(ctx, actor, "records.read")
	if e != nil {
		return out, e
	}
	rs := snap.Jobs[job]
	if len(rs) == 0 {
		return out, wrkrecords.Error("not_found")
	}
	tail := rs[len(rs)-1].Seq
	if snapshot == 0 {
		snapshot = tail
	}
	if after < 0 || snapshot < 1 || snapshot > tail || after > snapshot || limit < 1 || limit > 500 {
		return out, wrkrecords.Error("invalid_cursor")
	}
	history := []wrkrecords.Record{}
	for _, r := range rs {
		if r.Seq <= snapshot {
			history = append(history, r)
			if r.Seq > after && len(out.Records) < limit {
				out.Records = append(out.Records, r)
				out.Next = r.Seq
			}
		}
	}
	snap.Jobs[job] = history
	out.Snapshot = snapshot
	out.More = out.Next < snapshot
	out.Summary, e = snap.Summary(job, s.WrkEngine().Time())
	return out, e
}
func (s *Store) WrkContent(ctx context.Context, actor, job string, seq int64) (wrkrecords.ContentResponse, error) {
	snap, e := s.WrkSnapshot(ctx, actor, "content.read")
	if e != nil {
		return wrkrecords.ContentResponse{}, e
	}
	b := snap.Contents[job][seq]
	if len(b) == 0 {
		return wrkrecords.ContentResponse{}, wrkrecords.Error("not_found")
	}
	return wrkrecords.ContentResponse{JobID: job, Seq: seq, Base64: base64.StdEncoding.EncodeToString(b), SHA: wrkrecords.SHA(b), Size: len(b)}, nil
}

// Normalize SQL failures at the boundary; errors must never expose a query,
// body, path, credential or database detail to the transport.
func wrkError(err error) error {
	var e wrkrecords.Error
	if errors.As(err, &e) {
		return e
	}
	if err != nil {
		return wrkrecords.Error("wrk_unavailable")
	}
	return nil
}

// OpenRuntime connects to an already migrated schema. The migration owner is
// used by the desk's schema step; its credential is never needed by runtime.
func OpenRuntime(ctx context.Context, url string) (*Store, error) {
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	fail := func() (*Store, error) { p.Close(); return nil, wrkrecords.Error("wrk_unavailable") }
	if p.Ping(ctx) != nil {
		return fail()
	}
	var ready bool
	if p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=20)`).Scan(&ready) != nil || !ready {
		return fail()
	}
	return &Store{pool: p}, nil
}
func (s *Store) HasWrkEnrollment(ctx context.Context) bool {
	var yes bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrk_records WHERE kind='activate' OR (kind='claim' AND payload->'review_of'<>'null'::jsonb))`).Scan(&yes)
	return err == nil && yes
}

// OpenServer enforces the off-mode startup ruling and dr-1925-1. Both URLs
// are passed as credentials only; errors never contain either value.
func OpenServer(ctx context.Context, runtimeURL, migrationURL, mode string, policy *wrkrecords.Policy, generation string, recovery bool) (*Store, error) {
	enabled := mode != "" && mode != "off"
	if enabled && wrkrecords.DeskOwnedMigrationCredential && (migrationURL == "" || migrationURL == runtimeURL) {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	if enabled && (policy == nil || !wrkrecords.UUIDRE.MatchString(generation)) {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	if migrationURL != "" {
		if migrationURL == runtimeURL {
			return nil, wrkrecords.Error("wrk_unavailable")
		}
		owner, e := Open(ctx, migrationURL)
		if e != nil {
			return nil, wrkrecords.Error("wrk_unavailable")
		}
		e = owner.MigrateWrk(ctx)
		owner.Close()
		if e != nil {
			return nil, wrkrecords.Error("wrk_unavailable")
		}
	}
	// Inspect without DDL. An enrolled/runtime schema must never execute owner
	// migrations with the runtime credential, even when the API has been disabled.
	probe, e := pgxpool.New(ctx, runtimeURL)
	if e != nil {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	var exists bool
	e = probe.QueryRow(ctx, `SELECT to_regclass('wrk_records') IS NOT NULL OR to_regclass('wrk_contents') IS NOT NULL OR to_regclass('wrk_attempts') IS NOT NULL OR to_regclass('wrk_job_locks') IS NOT NULL`).Scan(&exists)
	probe.Close()
	if e != nil {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	var s *Store
	if exists {
		s, e = OpenRuntime(ctx, runtimeURL)
	} else {
		s, e = Open(ctx, runtimeURL)
	}
	if e != nil {
		return nil, wrkrecords.Error("wrk_unavailable")
	}
	if e = s.ConfigureWrk(ctx, mode, policy, generation, recovery); e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
