package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hk 3295: every resolution path writes one outbox row in the transaction of
// its state change, addressed to the asking lane, and the row's receipts move
// received then applied. Each test names the invariant it pins.

// failOutboxCommit makes every transaction that wrote a notification_outbox
// row fail at COMMIT, after all its other writes: a deferred constraint
// trigger raises when the transaction tries to commit. The state change and
// the outbox row must roll back together; the returned func removes the
// trigger so the same call can be retried.
func failOutboxCommit(t *testing.T, pool *pgxpool.Pool) (undo func()) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION hk_fail_outbox_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$`,
		`CREATE CONSTRAINT TRIGGER hk_fail_outbox_commit AFTER INSERT ON notification_outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION hk_fail_outbox_commit()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		if _, err := pool.Exec(ctx, `DROP TRIGGER hk_fail_outbox_commit ON notification_outbox`); err != nil {
			t.Fatal(err)
		}
	}
}

// outboxByPrefix returns every outbox row, sent or not, whose event id starts
// with prefix.
func outboxByPrefix(t *testing.T, pool *pgxpool.Pool, prefix string) []NotificationOutbox {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE starts_with(event_id,$1) ORDER BY id`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []NotificationOutbox
	for rows.Next() {
		var x NotificationOutbox
		if err := scanNotificationOutbox(rows, &x); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Invariant: a withdrawn or default_applied resolution notifies the asking
// lane exactly like an answer, the text names the state, a replayed delivery
// writes nothing new, and a commit failure takes the notice and the
// resolution back together.
func TestResolveNoticeWithdrawnAndDefaultApplied(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	cases := []struct {
		kind  string
		in    func(DecisionRequest) ResolveDecisionInput
		state string
	}{
		{DecisionRequestWithdrawn, func(r DecisionRequest) ResolveDecisionInput {
			return ResolveDecisionInput{RequestID: r.ID, Kind: DecisionRequestWithdrawn, Text: "no longer needed", Channel: DecisionChannelCLI, Actor: "director-1", EventKey: "w-" + r.ID}
		}, "withdrawn: no longer needed by director-1 via cli"},
		{DecisionRequestDefaultApplied, func(r DecisionRequest) ResolveDecisionInput {
			return ResolveDecisionInput{RequestID: r.ID, Kind: DecisionRequestDefaultApplied, Receipt: "hk:doc receipt/x", Channel: DecisionChannelCLI, Actor: "director-1", EventKey: "d-" + r.ID}
		}, "default_applied by director-1 via cli"},
	}
	for _, c := range cases {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)

		undo := failOutboxCommit(t, pool)
		if _, err := s.ResolveDecision(ctx, c.in(request)); err == nil {
			t.Fatalf("%s: commit failure was not reported", c.kind)
		}
		if rows := outboxByPrefix(t, pool, "decision-answered-"+request.ID+"-"); len(rows) != 0 {
			t.Fatalf("%s: rolled back resolution left %d notices", c.kind, len(rows))
		}
		live, _, _ := s.GetTask(ctx, task.ID)
		if live.Refs.DecisionRequest.Status != DecisionRequestOpen {
			t.Fatalf("%s: rollback left the request %s", c.kind, live.Refs.DecisionRequest.Status)
		}
		undo()

		out, err := s.ResolveDecision(ctx, c.in(request))
		if err != nil || out.Request.Status != c.kind {
			t.Fatalf("%s: out=%+v err=%v", c.kind, out, err)
		}
		rows := outboxByPrefix(t, pool, "decision-answered-"+request.ID+"-")
		if len(rows) != 1 || rows[0].TargetLane != task.Lane || rows[0].EventID != decisionAnswerNoticeEventID(request.ID, out.ResolutionID) {
			t.Fatalf("%s: rows=%+v", c.kind, rows)
		}
		if want := "[hk] 결정 답변 " + request.ID + " task " + strconv.FormatInt(task.ID, 10) + ": " + c.state; !strings.HasPrefix(rows[0].Text, want) {
			t.Fatalf("%s: text=%q want prefix %q", c.kind, rows[0].Text, want)
		}
		if _, err := s.ResolveDecision(ctx, c.in(request)); err != nil {
			t.Fatalf("%s: replay err=%v", c.kind, err)
		}
		if rows := outboxByPrefix(t, pool, "decision-answered-"+request.ID+"-"); len(rows) != 1 {
			t.Fatalf("%s: replay left %d notices", c.kind, len(rows))
		}
	}
}

// Invariant: the assistant resolve path notifies in its own transaction: a
// commit failure leaves no notice and an open request, the retry writes one.
func TestResolveAssistantNoticeRollsBack(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := DecisionAssistantResolveInput{RequestID: request.ID, Option: "A", Text: "x", EventKey: "assistant-rb-" + request.ID}

	undo := failOutboxCommit(t, pool)
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", in); err == nil {
		t.Fatal("commit failure was not reported")
	}
	if rows := outboxByPrefix(t, pool, "decision-answered-"+request.ID+"-"); len(rows) != 0 {
		t.Fatalf("rolled back resolve left %d notices", len(rows))
	}
	if live, _, _ := s.GetTask(ctx, task.ID); live.Refs.DecisionRequest.Status != DecisionRequestOpen {
		t.Fatalf("rollback left the request %s", live.Refs.DecisionRequest.Status)
	}
	undo()
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", in); err != nil {
		t.Fatal(err)
	}
	rows := outboxByPrefix(t, pool, "decision-answered-"+request.ID+"-")
	if len(rows) != 1 || rows[0].TargetLane != task.Lane {
		t.Fatalf("rows=%+v", rows)
	}
}

// Invariant: an operator deny of a pending attempt writes one notice to the
// task's lane in the deny transaction, named by the deny attempt; a replay
// and a refused second deny write none, and a commit failure takes the notice
// and the deny attempt back together.
func TestDecisionDenyNotice(t *testing.T) {
	s, pool := searchTestStore(t)
	s.SetConsoleURL("https://console.example.test")
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	request := humanOnlyRequest(t, s, task.ID)
	relay := answerA(request, DecisionChannelCLI, "session-a")
	relay.EventKey = "deny-notice-relay"
	pending, err := s.ResolveDecision(ctx, relay)
	if err != nil || pending.Result != DecisionResultPendingConfirmation {
		t.Fatalf("relay: %+v %v", pending, err)
	}
	deny := func(key string) ResolveDecisionInput {
		return ResolveDecisionInput{RequestID: request.ID, Kind: DecisionKindDeny, DeniesAttemptID: pending.AttemptID, Receipt: "relay was wrong", EventKey: key, Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true}
	}
	attempts := len(attemptsOf(t, pool, task.ID))

	undo := failOutboxCommit(t, pool)
	if _, err := s.ResolveDecision(ctx, deny("deny-notice-1")); err == nil {
		t.Fatal("commit failure was not reported")
	}
	if rows := outboxByPrefix(t, pool, "decision-denied-"+request.ID+"-"); len(rows) != 0 {
		t.Fatalf("rolled back deny left %d notices", len(rows))
	}
	if n := len(attemptsOf(t, pool, task.ID)); n != attempts {
		t.Fatalf("rolled back deny left attempts %d -> %d", attempts, n)
	}
	undo()

	out, err := s.ResolveDecision(ctx, deny("deny-notice-1"))
	if err != nil || out.Result != DecisionResultDenied {
		t.Fatalf("deny: %+v %v", out, err)
	}
	rows := outboxByPrefix(t, pool, "decision-denied-"+request.ID+"-")
	if len(rows) != 1 || rows[0].EventID != decisionDenyNoticeEventID(request.ID, out.AttemptID) || rows[0].Kind != OutboxKindDecisionDenied || rows[0].TargetLane != task.Lane {
		t.Fatalf("rows=%+v", rows)
	}
	wantText := "[hk] 결정 거부 " + request.ID + " task " + strconv.FormatInt(task.ID, 10) + ": attempt " + strconv.FormatInt(pending.AttemptID, 10) + " denied by operator:robin@example.com via console; request stays open | relay was wrong | https://console.example.test/ui/tasks/" + strconv.FormatInt(task.ID, 10)
	if rows[0].Text != wantText {
		t.Fatalf("text=%q want %q", rows[0].Text, wantText)
	}
	if _, err := s.ResolveDecision(ctx, deny("deny-notice-1")); err != nil {
		t.Fatalf("replay err=%v", err)
	}
	if _, err := s.ResolveDecision(ctx, deny("deny-notice-2")); !errors.Is(err, ErrDecisionAttemptDenied) {
		t.Fatalf("second deny err=%v", err)
	}
	if rows := outboxByPrefix(t, pool, "decision-denied-"+request.ID+"-"); len(rows) != 1 {
		t.Fatalf("replay and refusal left %d notices", len(rows))
	}
}

// Invariant: answering a disposition (an ESC item) writes one notice per
// answered lane event in the answering transaction, under the event id the
// console emits with; the single and the batch path both do, a commit failure
// takes the answer and the notice back together, and a skipped item writes
// none.
func TestDispositionAnswerNotice(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	merged := time.Now().UTC().Add(-time.Hour)
	item := func(lane, pr string) Task {
		in := DispositionInput{Lane: lane, Project: testProjectName, Title: "disposition " + pr, OriginPR: "https://github.com/example/outbox/pull/" + pr, MergeSHA: strings.Repeat("c", 40), MergedAt: &merged, Install: DispositionInstall{State: "unknown"}, Recommended: "A", CreatedBy: "director"}
		x, created, err := s.CreateDisposition(ctx, in)
		if err != nil || !created {
			t.Fatalf("create created=%v err=%v", created, err)
		}
		return x
	}
	gen := func(x Task) int64 {
		g, err := dispositionGen(ctx, pool, x.ID)
		if err != nil || g < 1 {
			t.Fatalf("gen=%d err=%v", g, err)
		}
		return g
	}

	single := item("outbox-single", "1")
	in := DispositionAnswerInput{ID: single.ID, Gen: gen(single), Key: "B", OperatorEmail: "robin@example.com", EventID: "web-disposition-" + strconv.FormatInt(single.ID, 10) + "-g1"}
	undo := failOutboxCommit(t, pool)
	if _, err := s.AnswerDisposition(ctx, in); err == nil {
		t.Fatal("commit failure was not reported")
	}
	if rows := outboxByPrefix(t, pool, in.EventID); len(rows) != 0 {
		t.Fatalf("rolled back answer left %d notices", len(rows))
	}
	if live, _, _ := s.GetTask(ctx, single.ID); !live.AwaitingDecision || live.Refs.Disposition.Answer != nil {
		t.Fatalf("rollback left the item answered: %+v", live.Refs.Disposition.Answer)
	}
	undo()
	answered, err := s.AnswerDisposition(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	wantText, err := DispositionEventText([]Task{answered})
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxByPrefix(t, pool, in.EventID)
	if len(rows) != 1 || rows[0].EventID != in.EventID || rows[0].Kind != OutboxKindDispositionAnswered || rows[0].TargetLane != "outbox-single" || rows[0].Text != wantText {
		t.Fatalf("rows=%+v want text %q", rows, wantText)
	}
	if _, err := s.AnswerDisposition(ctx, in); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second answer err=%v", err)
	}
	if rows := outboxByPrefix(t, pool, in.EventID); len(rows) != 1 {
		t.Fatalf("refused re-answer left %d notices", len(rows))
	}

	a1, a2, b1 := item("outbox-lane-a", "2"), item("outbox-lane-a", "3"), item("outbox-lane-b", "4")
	stale := item("outbox-lane-b", "5")
	refs := []DispositionRef{{ID: a1.ID, Gen: gen(a1)}, {ID: a2.ID, Gen: gen(a2)}, {ID: b1.ID, Gen: gen(b1)}, {ID: stale.ID, Gen: gen(stale) + 1000}}
	eventFor := func(lane string) string { return "web-disposition-batch-b1-" + lane }
	answeredBatch, skipped, err := s.AnswerDispositionBatch(ctx, refs, "robin@example.com", "b1", eventFor)
	if err != nil || len(answeredBatch) != 3 || len(skipped) != 1 {
		t.Fatalf("batch answered=%d skipped=%+v err=%v", len(answeredBatch), skipped, err)
	}
	for lane, ids := range map[string][]int64{"outbox-lane-a": {a1.ID, a2.ID}, "outbox-lane-b": {b1.ID}} {
		rows := outboxByPrefix(t, pool, eventFor(lane))
		if len(rows) != 1 || rows[0].TargetLane != lane || rows[0].Kind != OutboxKindDispositionAnswered {
			t.Fatalf("lane %s rows=%+v", lane, rows)
		}
		for _, id := range ids {
			if !strings.Contains(rows[0].Text, "#"+strconv.FormatInt(id, 10)+"=A") {
				t.Fatalf("lane %s text %q misses #%d", lane, rows[0].Text, id)
			}
		}
	}
	if rows := outboxByPrefix(t, pool, "web-disposition-batch-b1-"); len(rows) != 2 {
		t.Fatalf("batch wrote %d notices, want one per lane", len(rows))
	}
}

// Invariant: a deploy result notifies the lane that spawned the deploy's job,
// once per result: the structured finish writes it in the finish transaction
// (a commit failure leaves the attempt running and no row), a replayed finish
// writes nothing new, a deploy record document is a report of its own (never
// matched to an attempt by service/job/ref, so another target's result is not
// silenced): replayed it writes nothing new, corrected at the same key it
// notifies again, and control characters or NUL in its fields neither fail the
// document write nor make the notice undeliverable. A record without a ref or
// a retried attempt is a result of its own, ambiguous job/ref pairs never
// share an id, and a deploy with no job or no recorded lane writes none.
func TestDeployResultNotice(t *testing.T) {
	f := newDeployFixture(t, "outbox_deploy", "prod")
	pool := f.s.pool
	ctx := context.Background()
	claim := func(job, parent, owner string) {
		t.Helper()
		if _, _, err := f.s.ClaimJob(ctx, Job{JobID: job, Host: "m1b", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET parent_lane=$2,owner_lane=$3 WHERE job_id=$1`, job, parent, owner); err != nil {
			t.Fatal(err)
		}
	}
	startRef := func(attempt, job, ref string) Deploy {
		t.Helper()
		d, _, err := f.s.StartDeploy(ctx, DeployStart{AttemptID: attempt, Service: f.svc, Targets: []string{"prod"}, Action: DeployActionDeploy, DeployedRef: ref, JobID: job, Source: "mergewake"})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	start := func(attempt, job string) Deploy { return startRef(attempt, job, "ref-"+attempt) }
	claim("job-deploy-1", "asker-lane", "worker-lane")
	claim("job-deploy-2", "", "owner-only-lane")
	d := start("att-1", "job-deploy-1")
	id := deployResultAttemptEventID("att-1")

	undo := failOutboxCommit(t, pool)
	if _, err := f.s.FinishDeploy(ctx, "att-1", DeployFinish{State: DeploySuccess}); err == nil {
		t.Fatal("commit failure was not reported")
	}
	if rows := outboxByPrefix(t, pool, "deploy-result-"); len(rows) != 0 {
		t.Fatalf("rolled back finish left %d notices", len(rows))
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM deploys WHERE id=$1`, d.ID).Scan(&state); err != nil || state != DeployRequested {
		t.Fatalf("rollback left the attempt %q err=%v", state, err)
	}
	undo()

	f.finish(t, "att-1", DeploySuccess)
	rows := outboxByPrefix(t, pool, id)
	if len(rows) != 1 || rows[0].TargetLane != "asker-lane" || rows[0].Kind != OutboxKindDeployResult || rows[0].Text != "[hk] deploy "+f.svc+" ref-att-1 job job-deploy-1: success" {
		t.Fatalf("rows=%+v", rows)
	}
	f.finish(t, "att-1", DeploySuccess)
	if rows := outboxByPrefix(t, pool, id); len(rows) != 1 {
		t.Fatalf("replayed finish left %d notices", len(rows))
	}

	// A record document is its own report, identified by its key and the
	// SHA256 of its stored body: the same job, ref and result at another
	// target still notifies, replaying a body writes nothing new, and any
	// corrected body (result, step, target, job or ref) notifies once.
	body := func(target, ref, result, step, job string) string {
		return `{"schema":"` + DeployRecordSchema + `","service":"` + f.svc + `","target":"` + target + `","deployed_ref":` + ref + `,"result":"` + result + `",` + step + `"job_id":"` + job + `","included_prs":[]}`
	}
	put := func(key, text string) string {
		t.Helper()
		doc := Document{Key: key, Kind: "report", Session: "test", CreatedBy: "tester", Body: text}
		if _, _, err := f.s.PutDocument(ctx, doc); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		sum := sha256.Sum256([]byte(text))
		return deployResultRecordEventID(key, hex.EncodeToString(sum[:]))
	}
	rec := "deploy/" + f.svc + "/20261113T010203Z"
	recID := put(rec, body("staging", `"ref-att-1"`, "success", "", "job-deploy-1"))
	if again := put(rec, body("staging", `"ref-att-1"`, "success", "", "job-deploy-1")); again != recID {
		t.Fatal("same body, different id")
	}
	if rows := outboxByPrefix(t, pool, recID); len(rows) != 1 || rows[0].TargetLane != "asker-lane" {
		t.Fatalf("record rows=%+v (replay must add none)", rows)
	}
	if rows := outboxByPrefix(t, pool, id); len(rows) != 1 {
		t.Fatalf("record disturbed the attempt's notice: %d rows", len(rows))
	}
	// A correction at the same key notifies again: result, then failed step.
	failedID := put(rec, body("staging", `"ref-att-1"`, "failed", `"failed_step":2,`, "job-deploy-1"))
	step3ID := put(rec, body("staging", `"ref-att-1"`, "failed", `"failed_step":3,`, "job-deploy-1"))
	for step, eid := range map[string]string{"2": failedID, "3": step3ID} {
		if rows := outboxByPrefix(t, pool, eid); len(rows) != 1 || !strings.HasSuffix(rows[0].Text, ": failed at step "+step) {
			t.Fatalf("corrected record (step %s) rows=%+v", step, rows)
		}
	}
	// Bodies whose free-text fields would join to the same string still own
	// distinct ids (no NUL-joined identity), each on its own job's lane.
	claim("job-amb-a", "lane-amb-a", "")
	claim("job-amb-b", "lane-amb-b", "")
	ambKey := "deploy/" + f.svc + "/20261113T010253Z"
	ambA := put(ambKey, body("prod", `"b\u0000c"`, "failed", "", "job-amb-a"))
	ambB := put(ambKey, body(`prod\u0000job-amb-a`, `"c"`, "failed", "", "job-amb-b"))
	if ambA == ambB {
		t.Fatal("distinct bodies share an id")
	}
	for eid, lane := range map[string]string{ambA: "lane-amb-a", ambB: "lane-amb-b"} {
		if rows := outboxByPrefix(t, pool, eid); len(rows) != 1 || rows[0].TargetLane != lane {
			t.Fatalf("ambiguous-field record rows=%+v want lane %s", rows, lane)
		}
	}
	// Control characters in a field are mapped, NUL in the job id names no
	// job; the document write commits either way.
	ctlID := put("deploy/"+f.svc+"/20261113T010303Z", body("prod", `"ref\nwith\u0000nul"`, "success", "", "job-deploy-1"))
	if rows := outboxByPrefix(t, pool, ctlID); len(rows) != 1 || !validLaneEventText(rows[0].Text) {
		t.Fatalf("control character record rows=%+v", rows)
	}
	nulID := put("deploy/"+f.svc+"/20261113T010403Z", body("prod", `"ref-x"`, "success", "", `job-deploy-1\u0000x`))
	if rows := outboxByPrefix(t, pool, nulID); len(rows) != 0 {
		t.Fatalf("NUL job id queued %d notices", len(rows))
	}
	// A record of another ref is another result, and a failed one says so.
	docID := put("deploy/"+f.svc+"/20261113T020203Z", `{"schema":"`+DeployRecordSchema+`","service":"`+f.svc+`","target":"prod","deployed_ref":"ref-doc","result":"failed","failed_step":3,"job_id":"job-deploy-2","included_prs":null}`)
	if rows := outboxByPrefix(t, pool, docID); len(rows) != 1 || rows[0].TargetLane != "owner-only-lane" || rows[0].Text != "[hk] deploy "+f.svc+" ref-doc job job-deploy-2: failed at step 3" {
		t.Fatalf("record rows=%+v", rows)
	}

	// A record without a ref is still a result of its own.
	noRefID := put("deploy/"+f.svc+"/20261113T030203Z", `{"schema":"`+DeployRecordSchema+`","service":"`+f.svc+`","target":"prod","deployed_ref":null,"result":"failed","job_id":"job-deploy-2","included_prs":null}`)
	if rows := outboxByPrefix(t, pool, noRefID); len(rows) != 1 || rows[0].TargetLane != "owner-only-lane" || rows[0].Text != "[hk] deploy "+f.svc+" (no ref) job job-deploy-2: failed" {
		t.Fatalf("record without ref rows=%+v", rows)
	}

	// Distinct deploys never share an id: hyphen-ambiguous (job, ref) pairs
	// land on their own lanes, and a retried attempt of the same job and ref
	// is a result of its own.
	claim("a-b", "lane-ab", "")
	claim("a", "lane-a", "")
	startRef("att-amb-1", "a-b", "c")
	startRef("att-amb-2", "a", "b-c")
	f.finish(t, "att-amb-1", DeployFailed)
	f.finish(t, "att-amb-2", DeployFailed)
	startRef("att-retry-1", "a", "r")
	startRef("att-retry-2", "a", "r")
	f.finish(t, "att-retry-1", DeployFailed)
	f.finish(t, "att-retry-2", DeployFailed)
	for attempt, lane := range map[string]string{"att-amb-1": "lane-ab", "att-amb-2": "lane-a", "att-retry-1": "lane-a", "att-retry-2": "lane-a"} {
		if rows := outboxByPrefix(t, pool, deployResultAttemptEventID(attempt)); len(rows) != 1 || rows[0].TargetLane != lane {
			t.Fatalf("attempt %s rows=%+v want lane %s", attempt, rows, lane)
		}
	}

	// No job, or a job no lane is recorded for: nobody asked, nothing queued.
	start("att-nojob", "")
	f.finish(t, "att-nojob", DeployFailed)
	claim("job-deploy-3", "", "")
	start("att-nolane", "job-deploy-3")
	f.finish(t, "att-nolane", DeployRolledBack)
	start("att-unknown-job", "job-never-claimed")
	f.finish(t, "att-unknown-job", DeployFailed)
	for _, attempt := range []string{"att-nojob", "att-nolane", "att-unknown-job"} {
		if rows := outboxByPrefix(t, pool, deployResultAttemptEventID(attempt)); len(rows) != 0 {
			t.Fatalf("attempt %s with no asking lane wrote %+v", attempt, rows)
		}
	}
}

// Invariant: an outbox row's receipts move received then applied. The hub's
// receipt comes first; a lane's ack before it is refused and writes nothing,
// the ack is idempotent for the same identity and a conflict for another, an
// unknown event is not found, and the table itself refuses applied without
// received.
func TestOutboxReceiptsReceivedThenApplied(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionAnswered, "receipt-lane", "receipt-event-1", "text", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := s.MarkNotificationApplied(ctx, "receipt-event-1", "receipt-lane"); !errors.Is(err, ErrOutboxNotReceived) {
		t.Fatalf("applied before received err=%v", err)
	}
	if _, err := s.MarkNotificationApplied(ctx, "receipt-event-none", "receipt-lane"); !errors.Is(err, ErrOutboxEventNotFound) {
		t.Fatalf("unknown event err=%v", err)
	}
	if _, err := s.MarkNotificationApplied(ctx, "receipt-event-1", ""); err == nil {
		t.Fatal("an ack without an identity was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE notification_outbox SET applied_at=now() WHERE event_id='receipt-event-1'`); err == nil {
		t.Fatal("the table accepted applied without received")
	}
	row := outboxByPrefix(t, pool, "receipt-event-1")[0]
	if row.SentAt != nil || row.AppliedAt != nil {
		t.Fatalf("refused acks left receipts: %+v", row)
	}

	received, err := s.MarkNotificationSent(ctx, "receipt-event-1", 77)
	if err != nil || received.SentAt == nil || received.AppliedAt != nil {
		t.Fatalf("received: %+v err=%v", received, err)
	}
	applied, err := s.MarkNotificationApplied(ctx, "receipt-event-1", "receipt-lane")
	if err != nil || applied.AppliedAt == nil || applied.AppliedBy == nil || *applied.AppliedBy != "receipt-lane" || applied.SentAt == nil || applied.AppliedAt.Before(*applied.SentAt) {
		t.Fatalf("applied: %+v err=%v", applied, err)
	}
	again, err := s.MarkNotificationApplied(ctx, "receipt-event-1", "receipt-lane")
	if err != nil || !again.AppliedAt.Equal(*applied.AppliedAt) {
		t.Fatalf("repeat ack: %+v err=%v", again, err)
	}
	if _, err := s.MarkNotificationApplied(ctx, "receipt-event-1", "other-lane"); !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("ack by another identity err=%v", err)
	}
	if rows, err := s.ListUnsentNotifications(ctx, 0); err != nil {
		t.Fatal(err)
	} else {
		for _, r := range rows {
			if r.EventID == "receipt-event-1" {
				t.Fatal("a received row is still listed as unsent")
			}
		}
	}
}

// laneSafeText leaves a deploy field deliverable: control characters and
// invalid UTF-8 never reach the lane-event contract or a PostgreSQL text
// parameter, and distinct record fields name distinct event ids.
func TestLaneSafeTextAndRecordEventID(t *testing.T) {
	got := laneSafeText("a\nb\x00c\x7f\xffd")
	if !validLaneEventText("[hk] "+got) || strings.ContainsAny(got, "\n\x00\x7f") || !utf8.ValidString(got) {
		t.Fatalf("laneSafeText=%q", got)
	}
	if laneSafeText("plain ref-1 한글") != "plain ref-1 한글" {
		t.Fatal("laneSafeText changed a clean string")
	}
	base := deployResultRecordEventID("k", "aa")
	for _, other := range []string{deployResultRecordEventID("k", "ab"), deployResultRecordEventID("k2", "aa"), deployResultRecordEventID("ka", "a")} {
		if other == base {
			t.Fatal("distinct records share an event id")
		}
	}
	if base != deployResultRecordEventID("k", "aa") {
		t.Fatal("event id is not stable")
	}
}
