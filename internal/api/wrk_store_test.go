package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// Store-backed O3 suite: the same pack vectors and fences against
// PostgreSQL. It needs HANDOFFKEEP_TEST_DB_URL, which only CI provides; in
// CI a missing URL is a failure, never a skip.

type pgFixture struct {
	t          *testing.T
	admin      *pgx.Conn
	schema     string
	role       string
	ownerURL   string
	runtimeURL string
	st         *store.Store
}

func testDBURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if base == "" {
		if inCI() {
			t.Fatal("CI must provide HANDOFFKEEP_TEST_DB_URL: store-backed O3 vectors may not skip in CI")
		}
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for store-backed O3 tests (CI only)")
	}
	return base
}

func withSearchPath(t *testing.T, base, schema, user, password string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String()
}

// newPGFixture builds a throwaway schema whose legacy tables are granted to
// a fresh runtime role, then opens the store through OpenWithOwner so the
// journal schema is created by the owner and the runtime role holds only
// the journal grants OpenWithOwner gives it.
func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	base := testDBURL(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	nano := time.Now().UnixNano()
	fx := &pgFixture{t: t, admin: admin, schema: fmt.Sprintf("wrk_%d", nano), role: fmt.Sprintf("wrkrt_%d", nano)}
	password := randomToken(t)
	for _, q := range []string{
		`CREATE SCHEMA ` + fx.schema,
		`CREATE ROLE ` + fx.role + ` LOGIN PASSWORD '` + password + `'`,
		`GRANT USAGE ON SCHEMA ` + fx.schema + ` TO ` + fx.role,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if fx.st != nil {
			fx.st.Close()
		}
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+fx.schema+` CASCADE`)
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+fx.role)
		_, _ = admin.Exec(context.Background(), `DROP ROLE `+fx.role)
		_ = admin.Close(context.Background())
	})
	fx.ownerURL = withSearchPath(t, base, fx.schema, "", "")
	fx.runtimeURL = withSearchPath(t, base, fx.schema, fx.role, password)
	legacy, err := store.Open(ctx, fx.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	for _, q := range []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + fx.schema + ` TO ` + fx.role,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA ` + fx.schema + ` TO ` + fx.role,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if fx.st, err = store.OpenWithOwner(ctx, fx.runtimeURL, fx.ownerURL); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `SET search_path TO `+fx.schema+`,public`); err != nil {
		t.Fatal(err)
	}
	return fx
}

// reset empties the journal and task tables between vectors. Only the
// test's superuser connection can, with append-only triggers suspended for
// its own session.
func (fx *pgFixture) reset() {
	fx.t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`SET session_replication_role = replica`,
		`TRUNCATE wrk_contents, wrk_records, wrk_attempts, wrk_job_locks, relay_events, task_events, tasks RESTART IDENTITY CASCADE`,
		`RESET session_replication_role`,
	} {
		if _, err := fx.admin.Exec(ctx, q); err != nil {
			fx.t.Fatal(err)
		}
	}
}

func (fx *pgFixture) addTask(id int64, state string) {
	fx.t.Helper()
	if _, err := fx.admin.Exec(context.Background(), `INSERT INTO tasks(id,lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,project) VALUES($1,'o3-lane','','o3 task','implement',$2,0,'{}'::jsonb,'','test',now(),now(),'handoffkeep')`, id, state); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *pgFixture) seed(r wrk.Record, content []byte) {
	fx.t.Helper()
	ctx := context.Background()
	payload, _ := json.Marshal(r.Payload)
	var source any
	if r.Source != nil {
		b, _ := json.Marshal(r.Source)
		source = string(b)
	}
	at, err := time.Parse(time.RFC3339Nano, r.ReceivedAt)
	if err != nil {
		fx.t.Fatal(err)
	}
	if _, err := fx.admin.Exec(ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1) ON CONFLICT DO NOTHING`, r.JobID); err != nil {
		fx.t.Fatal(err)
	}
	if _, err := fx.admin.Exec(ctx, `INSERT INTO wrk_records(job_id,seq,record_id,store_generation,claim_seq,actor_client_id,idempotency_key,request_sha256,imported,source,received_at,previous_hash,record_hash,kind,payload) VALUES($1,$2,$3::uuid,$4::uuid,$5,$6,$7::uuid,$8,$9,$10::jsonb,$11,$12,$13,$14,$15::jsonb)`,
		r.JobID, r.Seq, r.RecordID, r.StoreGeneration, r.ClaimSeq, r.ActorClientID, r.IdempotencyKey, r.RequestSHA256, r.Imported, source, at, r.PreviousHash, r.RecordHash, r.Kind, string(payload)); err != nil {
		fx.t.Fatalf("seed %s#%d: %v", r.JobID, r.Seq, err)
	}
	if content != nil {
		if _, err := fx.admin.Exec(ctx, `INSERT INTO wrk_contents(job_id,seq,content,content_sha256,size_bytes) VALUES($1,$2,$3,$4,$5)`, r.JobID, r.Seq, content, wrk.ContentSHA256(content), len(content)); err != nil {
			fx.t.Fatal(err)
		}
	}
}

func (fx *pgFixture) recordCount(job string) int {
	var n int
	if err := fx.admin.QueryRow(context.Background(), `SELECT count(*) FROM wrk_records WHERE job_id=$1`, job).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

// newPackEnvPG resets the fixture and seeds one vector's given history.
func newPackEnvPG(t *testing.T, fx *pgFixture, c *packCase, legacy map[string][]byte) *packEnv {
	t.Helper()
	fx.t = t
	fx.reset()
	policy, err := wrk.ParsePolicy(c.Given.Policy)
	if err != nil {
		t.Fatal(err)
	}
	checkGivenHistory(t, c, policy)
	for _, id := range packTaskIDs(c) {
		fx.addTask(id, "backlog")
	}
	for _, r := range c.Given.Records {
		fx.seed(r, seedContent(t, legacy, r))
	}
	for _, r := range c.Given.Records {
		if r.Kind == "claim" && (r.Payload["role"] == "builder" || r.Payload["role"] == "worker") {
			if _, err := fx.admin.Exec(context.Background(), `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text) WHERE id=$1`, r.Payload["task_id"], r.JobID); err != nil {
				t.Fatal(err)
			}
		}
	}
	tokens := Tokens{}
	byClient := map[string]string{}
	for _, id := range append(append([]string{}, wrk.ClientIDs...), "unknown-host") {
		tok := randomToken(t)
		tokens[id] = tok
		byClient[id] = tok
	}
	eng := &wrk.Engine{Policy: policy, Generation: packGeneration, Enforce: true, Now: func() time.Time { return packNow }}
	s := Server{Service: Service{Store: fx.st}, Tokens: tokens, Wrk: &Wrk{Mode: WrkEnforce, Policy: policy, Engine: eng, Backend: fx.st.WrkJournal()}}
	return &packEnv{t: t, backend: s.Wrk.Backend, server: s, handler: s.Handler(), tokens: byClient, policy: policy, records: func(job string) []wrk.Record {
		return make([]wrk.Record, fx.recordCount(job))
	}}
}

func (e *packEnv) legacyPost(actor, path string, body any) packResponse {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(actor, "POST", path, nil, b)
}

// receiptOf builds the legacy receipt tuple of a seeded record.
func receiptOf(c *packCase, job string, seq int64) map[string]any {
	for _, r := range c.Given.Records {
		if r.JobID == job && r.Seq == seq {
			return map[string]any{"job_id": job, "seq": seq, "record_hash": r.RecordHash, "store_generation": r.StoreGeneration}
		}
	}
	return nil
}

// legacyAdapters run each legacy vector as today's real request for its
// route. The pack's legacy bodies are schematic (route and intent), so each
// adapter supplies the current wire shape and keeps the vector's actor,
// route, protected task/job and expected outcome.
var legacyAdapters = map[string]func(e *packEnv, fx *pgFixture, c *packCase) packResponse{
	"legacy-route-fence.json/tasks-create-with-o3-refs-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks", map[string]any{"lane": "o3-lane", "title": "x", "kind": "implement", "project": "handoffkeep", "refs": map[string]any{"job_id": "job-a"}})
	},
	"legacy-route-fence.json/tasks-create-unrelated-legacy-compatible": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks", map[string]any{"lane": "o3-lane", "title": "x", "kind": "implement", "project": "handoffkeep", "refs": map[string]any{}})
	},
	// Mapping: a real lane plus an explicit job_id naming the protected O3
	// job, which is the explicit claim attempt (esc-2 inference 14).
	"legacy-route-fence.json/tasks-next-claim-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/next", map[string]any{"lane": "o3-lane", "claimed_by": "desktop-pane", "job_id": "job-a"})
	},
	"legacy-route-fence.json/tasks-claim-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/2400/claim", map[string]any{"claimed_by": "director-1", "job_id": "job-a"})
	},
	"legacy-route-fence.json/tasks-rebind-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/2400/rebind", map[string]any{"from_job": "job-a", "to_job": "job-z", "by": "desktop-pane", "reason": "takeover"})
	},
	"legacy-route-fence.json/tasks-transition-binding-change-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/2400/transition", map[string]any{"to": "claimed", "refs": map[string]any{"job_id": "job-z"}})
	},
	"legacy-route-fence.json/tasks-transition-state-only-without-receipt-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "merged"})
	},
	"legacy-route-fence.json/tasks-project-or-relane-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		r := e.legacyPost("desktop", "/v1/tasks/relane", map[string]any{"ids": []int64{2400}, "to": "other", "note": "move", "allow_new_lane": true})
		return batchItem(r)
	},
	"legacy-route-fence.json/decision-request-resolve-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/2400/decision-request/resolve", map[string]any{"request_id": "dr-2400-1", "kind": "answered", "option": "A"})
	},
	// Mapping (director-1 ruling on schematic legacy bodies): task 2400, the
	// protected subject, is seeded as a real answered disposition item, and
	// the route's real body ({"note"}) is sent. Without the fence the apply
	// would run its hops.
	"legacy-route-fence.json/dispositions-apply-binding-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		fx.seedAnsweredDisposition(2400)
		return e.legacyPost("desktop", "/v1/tasks/dispositions/2400/apply", map[string]any{"note": "apply"})
	},
	"legacy-route-fence.json/decisions-resolve-protected-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		fx.setTaskState(2400, "needs_decision")
		return e.legacyPost("desktop", "/v1/decisions/resolve", map[string]any{"type": "task", "id": 2400, "by": "desktop-pane", "answer": "a"})
	},
	"legacy-route-fence.json/relay-terminal-without-receipt-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.joined", "job_id": "job-a", "owner_lane": "b2400-a", "head": head('1')})
	},
	"transition.json/legacy-relay-completed-no-receipt": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.completed", "job_id": "job-a", "owner_lane": "b2400-a", "head": head('1'), "report_sha256": wrk.ContentSHA256(packReportBytes)})
	},
	"transition.json/legacy-task-no-job-bypass-denied": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/tasks/2400/transition", map[string]any{"to": "dropped", "no_job": "bypass"})
	},
	"failure-abuse.json/deleted-local-events-canonical-unchanged": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.completed", "job_id": "job-a", "owner_lane": "b2400-a", "head": head('1'), "report_sha256": wrk.ContentSHA256(packReportBytes)})
	},
	"failure-abuse.json/legacy-joined-direct-or-task-relink-fence": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.joined", "job_id": "job-a", "owner_lane": "b2400-a", "head": head('1')})
	},
	"r4-review-lifecycle.json/r-legacy-receiptless-terminal-relay": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.completed", "job_id": "job-a-t1", "owner_lane": "b2400-a", "head": head('1'), "report_sha256": wrk.ContentSHA256(packReportBytes)})
	},
	"r4-review-lifecycle.json/r-end-receipt-cannot-drop-subject-task": func(e *packEnv, fx *pgFixture, c *packCase) packResponse {
		return e.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "dropped", "wrk_receipt": receiptOf(c, "job-a-t1", 2)})
	},
}

// seedAnsweredDisposition turns task id into a claimed disposition item
// carrying an operator answer (the state ApplyDisposition acts on), keeping
// its O3 job binding.
func (fx *pgFixture) seedAnsweredDisposition(id int64) {
	fx.t.Helper()
	now := time.Now().UTC()
	var refs store.TaskRefs
	var raw []byte
	if err := fx.admin.QueryRow(context.Background(), `SELECT refs FROM tasks WHERE id=$1`, id).Scan(&raw); err != nil {
		fx.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		fx.t.Fatal(err)
	}
	refs.OriginTask = id - 1
	refs.Disposition = &store.Disposition{Schema: store.DispositionSchema, Recommended: "A",
		Facts:  store.DispositionFacts{FactsSource: "hk-task", FactsAsOf: now, Install: store.DispositionInstall{State: "unknown"}},
		Answer: &store.DispositionAnswer{Key: "A", By: "operator:test", EventID: "web-disposition-test", Gen: 1, At: now}}
	b, _ := json.Marshal(refs)
	if _, err := fx.admin.Exec(context.Background(), `UPDATE tasks SET kind='decide', state='claimed', refs=$2::jsonb WHERE id=$1`, id, string(b)); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *pgFixture) setTaskState(id int64, state string) {
	fx.t.Helper()
	if _, err := fx.admin.Exec(context.Background(), `UPDATE tasks SET state=$2 WHERE id=$1`, id, state); err != nil {
		fx.t.Fatal(err)
	}
}

// batchItem lifts the first item of a relane batch reply into a response
// with the item's code (the batch route answers 200 with per-item codes).
func batchItem(r packResponse) packResponse {
	results, _ := r.body["results"].([]any)
	if r.status != 200 || len(results) != 1 {
		return r
	}
	item := results[0].(map[string]any)
	if code, _ := item["error"].(string); code != "" {
		status, _ := wrk.Status(wrk.Code(code))
		return packResponse{status: status, body: map[string]any{"error": code}}
	}
	return packResponse{status: 200, body: item}
}

func TestO3PackStore(t *testing.T) {
	fx := newPGFixture(t)
	legacy := legacyBySHA(t)
	for _, c := range loadPackCases(t) {
		c := c
		switch c.reqType() {
		case "http", "http_raw":
			t.Run("http/"+c.id(), func(t *testing.T) {
				checkHTTPVector(t, c, func(t *testing.T, c *packCase) *packEnv { return newPackEnvPG(t, fx, c, legacy) })
			})
		case "legacy":
			t.Run("legacy/"+c.id(), func(t *testing.T) {
				adapter := legacyAdapters[c.id()]
				if adapter == nil {
					t.Fatalf("no adapter for legacy vector %s", c.id())
				}
				env := newPackEnvPG(t, fx, c, legacy)
				r := adapter(env, fx, c)
				if outcome, _ := c.Expect["outcome"].(string); outcome == "legacy_compatible" {
					if r.status != 201 {
						t.Fatalf("unrelated legacy task create: %d %s", r.status, r.raw)
					}
					return
				}
				want, _ := c.Expect["http_status"].(json.Number).Int64()
				if int64(r.status) != want || r.body["error"] != c.Expect["error"] {
					t.Fatalf("got %d %v, vector says %d %v", r.status, r.body, want, c.Expect["error"])
				}
			})
		case "meta":
			if metaStoreRules[c.Name] {
				t.Run("meta/"+c.id(), func(t *testing.T) { testProtectedReceipts(t, fx) })
			}
		}
	}
}

func TestO3PackLegacyAdaptersComplete(t *testing.T) {
	n := 0
	for _, c := range loadPackCases(t) {
		if c.reqType() != "legacy" {
			continue
		}
		n++
		if legacyAdapters[c.id()] == nil {
			t.Errorf("legacy vector %s has no adapter", c.id())
		}
	}
	if n != len(legacyAdapters) {
		t.Errorf("%d legacy vectors, %d adapters", n, len(legacyAdapters))
	}
}

// pgScenario is a scenario over the fixture's PostgreSQL journal.
func pgScenario(t *testing.T, fx *pgFixture) *scenario {
	t.Helper()
	c := &packCase{File: "scenario", Name: t.Name()}
	c.Given.Policy = standardPolicy(t)
	env := newPackEnvPG(t, fx, c, nil)
	for id := int64(2400); id <= 2420; id++ {
		fx.addTask(id, "backlog")
	}
	recs := func(job string) []wrk.Record {
		var out []wrk.Record
		_ = env.backend.View(context.Background(), func(tx wrk.Tx) error {
			j, err := tx.Journal(job)
			if err == nil {
				out = j.Records
			}
			return err
		})
		return out
	}
	env.records = recs
	return &scenario{packEnv: env}
}

// testProtectedReceipts is rule-09 (protected transitions and legacy
// relay/completion paths need canonical receipts) and the relay half of
// mutant "let a legacy relay job.completed through for a receipt_required
// job without a receipt".
func testProtectedReceipts(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.activeBuilder("job-p9", "b2400-p", 2400, "T1")
	done := s.ok(s.post("desktop", "job-p9", "completed", map[string]any{"report_b64": reportPayload(head('1'), nil)["report_b64"], "report_sha256": wrk.ContentSHA256(packReportBytes)}), "canonical completed")
	rec := done["record"].(map[string]any)
	receipt := map[string]any{"job_id": "job-p9", "seq": rec["seq"], "record_hash": rec["record_hash"], "store_generation": rec["store_generation"]}
	relay := map[string]any{"kind": "job.completed", "job_id": "job-p9", "owner_lane": "b2400-p", "report_sha256": wrk.ContentSHA256(packReportBytes)}
	s.refused(s.legacyPost("desktop", "/v1/relay/events", relay), "wrk_receipt_required", "receipt-less relay")
	withReceipt := map[string]any{}
	for k, v := range relay {
		withReceipt[k] = v
	}
	withReceipt["wrk_receipt"] = receipt
	r := s.legacyPost("desktop", "/v1/relay/events", withReceipt)
	if r.status != 200 || r.body["reason"] != store.WrkProjectionReason(int64(num(rec["seq"]))) {
		t.Fatalf("receipt relay did not dedup to the projection: %d %s", r.status, r.raw)
	}
	s.refused(s.legacyPost("director-1", "/v1/relay/events", withReceipt), "scope_denied", "central relay publication")
	bad := map[string]any{}
	for k, v := range withReceipt {
		bad[k] = v
	}
	bad["wrk_receipt"] = map[string]any{"job_id": "job-p9", "seq": rec["seq"], "record_hash": strings.Repeat("0", 64), "store_generation": rec["store_generation"]}
	s.refused(s.legacyPost("desktop", "/v1/relay/events", bad), "wrk_receipt_required", "forged hash")
	var projections int
	if err := fx.admin.QueryRow(context.Background(), `SELECT count(*) FROM relay_events WHERE job_id='job-p9' AND kind='job.completed'`).Scan(&projections); err != nil {
		t.Fatal(err)
	}
	if projections != 1 {
		t.Fatalf("%d completed relay rows, want exactly the canonical projection", projections)
	}
	// Unrelated legacy relays keep today's behaviour.
	if r := s.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.completed", "job_id": "legacy-job", "owner_lane": "lane-x"}); r.status != 201 {
		t.Fatalf("unrelated relay: %d %s", r.status, r.raw)
	}
	// State-only transitions: the bound subject's canonical receipt.
	claim := s.records("job-p9")[0]
	s.refused(s.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "claimed"}), "wrk_receipt_required", "central without receipt")
	claimReceipt := map[string]any{"job_id": "job-p9", "seq": claim.Seq, "record_hash": claim.RecordHash, "store_generation": claim.StoreGeneration}
	s.refused(s.legacyPost("desktop", "/v1/tasks/2400/transition", map[string]any{"to": "claimed", "wrk_receipt": claimReceipt}), "scope_denied", "host nonterminal state change")
	s.refused(s.legacyPost("desktop", "/v1/tasks/2400/transition", map[string]any{"to": "dropped", "wrk_receipt": claimReceipt}), "wrk_receipt_required", "host terminal state change")
	s.refused(s.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "dropped", "wrk_receipt": claimReceipt}), "wrk_receipt_required", "claim receipt for dropped")
	if r := s.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "claimed", "no_job": "o3 subject", "wrk_receipt": claimReceipt}); r.status != 200 {
		t.Fatalf("central state-only with current claim receipt: %d %s", r.status, r.raw)
	}
}

func TestO3StoreSuite(t *testing.T) {
	fx := newPGFixture(t)
	t.Run("RuntimeRoleCannotRewriteJournal", func(t *testing.T) { testRuntimeRoleAppendOnly(t, fx) })
	t.Run("LegacyFenceRows", func(t *testing.T) { testLegacyFenceRows(t, fx) })
	t.Run("ConcurrentAppendsOneJob", func(t *testing.T) { testConcurrentAppends(t, fx) })
	t.Run("EndRacesVerdict", func(t *testing.T) { testEndRacesVerdict(t, fx) })
	t.Run("CrossJobLockOrder", func(t *testing.T) { testCrossJobLockOrder(t, fx) })
	t.Run("LostReplyReplay", func(t *testing.T) { testLostReplyReplayPG(t, fx) })
	t.Run("ShadowProtectionStartsAtActivate", func(t *testing.T) { testShadowProtectionStartsAtActivate(t, fx) })
	t.Run("ProjectionUpsertAndHydration", func(t *testing.T) { testProjectionUpsertAndHydration(t, fx) })
	t.Run("ReceiptPhaseBoundary", func(t *testing.T) { testReceiptPhaseBoundary(t, fx) })
}
