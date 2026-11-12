package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Decision mode (hk 4054): the decision_issues feed consumer prepares a
// decision's decision_create op while it is unbound and one decision_fields
// op per changed snapshot once bound.

const (
	ytdecProjectID = "0-12"
	ytdecIssue     = "2-900"
)

func ytdecPass(t *testing.T, s *Store) YTFeedPass {
	t.Helper()
	return ytFeedPassOK(t, s, ytFeedConsumers[ytDecisionsConsumer])
}

// ytdecSetup is a throwaway store in decisions mode on (on is false: off) with
// the consumer's cursor already created.
func ytdecSetup(t *testing.T, prefix string, on bool) *Store {
	t.Helper()
	s := schemaStore(t, prefix)
	if on {
		s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, ytdecProjectID, "DEC")
	} else {
		s.SetYTDecisionsMode(YTDecisionsOff, "", "", "")
	}
	if pass := ytdecPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	return s
}

// ytdecChat opens a chat question and returns its decision's id.
func ytdecChat(t *testing.T, s *Store, qid, body string) int64 {
	t.Helper()
	if _, _, err := s.UpsertChatQuestion(t.Context(), ChatQuestion{ID: qid, Lane: "ytd-lane", Body: body}); err != nil {
		t.Fatal(err)
	}
	return onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, qid), DecisionStatusOpen).ID
}

func ytdecOps(t *testing.T, s *Store, kind string, id int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='decision' AND entity_id=$2 ORDER BY id`,
		kind, strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// ytdecApplyCreate dispatches and applies a decision's create op as the
// protocol does after a create returned ytID.
func ytdecApplyCreate(t *testing.T, s *Store, op ytwrite.Op, ytID string) ytwrite.Op {
	t.Helper()
	now := time.Now().UTC()
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), now, op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim create: %s %v", out, err)
	}
	got, err := s.YTWriteLedger().Apply(t.Context(), now, op.ID, ytID, []ytwrite.MapRow{{EntityKind: "decision", HKKey: op.EntityID, YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ytdecSnapshot(t *testing.T, op ytwrite.Op) ytwrite.IssueFieldsPayload {
	t.Helper()
	var p ytwrite.IssueFieldsPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ytdecField(p ytwrite.IssueFieldsPayload, name string) string {
	for _, f := range p.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return "<absent>"
}

// A new decision makes one create op; applying it binds the decision and
// prepares its first snapshot; a write that leaves the snapshot as it was
// makes none; the answer makes one fields op.
func TestYTDecisionsNewThenAnswerThenUnchanged(t *testing.T) {
	s := ytdecSetup(t, "ytd_flow", true)
	long := strings.Repeat("질문 전문이 YouTrack 에 그대로 간다. ", 20)
	id := ytdecChat(t, s, "Q-20261112-01", long)
	ytdecPass(t, s)
	creates := ytdecOps(t, s, ytwrite.KindDecisionCreate, id)
	if len(creates) != 1 || len(ytdecOps(t, s, ytwrite.KindDecisionFields, id)) != 0 {
		t.Fatalf("creates %+v", creates)
	}
	create := creates[0]
	if create.State != ytwrite.StatePrepared || create.Key != ytwrite.DecisionKey(id) || create.YTTargetID != ytdecProjectID ||
		create.Tenant != ytcTenant || create.Principal != YTDecisionsPrincipal {
		t.Fatalf("create %+v", create)
	}
	var cp ytwrite.DecisionCreatePayload
	if err := json.Unmarshal(create.Payload, &cp); err != nil || cp.Decision.Question != strings.TrimSpace(long) || cp.YTProjectShort != "DEC" {
		t.Fatalf("create payload %+v %v", cp, err)
	}
	// Another pass with no change prepares nothing more.
	ytdecPass(t, s)
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionCreate, id)); n != 1 {
		t.Fatalf("%d creates after an idle pass", n)
	}

	applied := ytdecApplyCreate(t, s, create, ytdecIssue)
	if applied.State != ytwrite.StateApplied || applied.Reason != "" {
		t.Fatalf("applied %+v", applied)
	}
	b, found, err := s.YTDecisionBinding(t.Context(), id)
	if err != nil || !found || b.IssueID != ytdecIssue || b.Tenant != ytcTenant || b.Origin != "op_applied" || b.OpID != create.ID || b.ProjectID != ytdecProjectID {
		t.Fatalf("binding %+v %v %v", b, found, err)
	}
	fields := ytdecOps(t, s, ytwrite.KindDecisionFields, id)
	if len(fields) != 1 || fields[0].YTTargetID != ytdecIssue {
		t.Fatalf("on-bind snapshot %+v", fields)
	}
	first := ytdecSnapshot(t, fields[0])
	if ytdecField(first, ytwrite.FieldDecisionState) != "열림" || ytdecField(first, ytwrite.FieldLane) != "ytd-lane" || ytdecField(first, ytwrite.FieldDecisionID) != strconv.FormatInt(id, 10) {
		t.Fatalf("first snapshot %+v", first)
	}
	if x, ok := ytwrite.ParseDecision(first.Description); !ok || x.Question != strings.TrimSpace(long) {
		t.Fatalf("description %q", first.Description)
	}
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), fields[0].ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim fields: %s %v", out, err)
	}

	// A snooze writes the row but not the snapshot: no op.
	if _, err := s.SnoozeDecisionRecord(t.Context(), id, time.Now().UTC().Add(time.Hour), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if pass := ytdecPass(t, s); pass.Handled != 1 {
		t.Fatalf("snooze pass %+v", pass)
	}
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionFields, id)); n != 1 {
		t.Fatalf("%d fields ops after an unchanged snapshot", n)
	}

	// The answer: one fields op with the answered state and who answered.
	if _, err := s.TransitionChatQuestion(t.Context(), "Q-20261112-01", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	ytdecPass(t, s)
	fields = ytdecOps(t, s, ytwrite.KindDecisionFields, id)
	if len(fields) != 2 {
		t.Fatalf("fields ops %+v", fields)
	}
	answered := ytdecSnapshot(t, fields[1])
	if ytdecField(answered, ytwrite.FieldDecisionState) != "답변됨" || !strings.Contains(answered.Description, "답한 사람: desk") ||
		!strings.Contains(answered.Description, "답한 시각: ") {
		t.Fatalf("answered snapshot %+v", answered)
	}
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionCreate, id)); n != 1 {
		t.Fatalf("%d creates", n)
	}
}

func TestYTDecisionsModeOffPreparesNothing(t *testing.T) {
	s := ytdecSetup(t, "ytd_off", false)
	id := ytdecChat(t, s, "Q-20261112-02", "off?")
	pass := ytdecPass(t, s)
	if pass.Read == 0 || pass.Cursor != ytFeedMaxSyncID(t, s) {
		t.Fatalf("pass %+v", pass)
	}
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionCreate, id)) + len(ytdecOps(t, s, ytwrite.KindDecisionFields, id)); n != 0 {
		t.Fatalf("%d ops in mode off", n)
	}
	// Mode off admits nothing either: an op prepared earlier is held.
	s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, ytdecProjectID, "DEC")
	ytdecChat(t, s, "Q-20261112-03", "on now")
	ytdecPass(t, s)
	var op ytwrite.Op
	for _, r := range decisionRowsOf(t, s, DecisionSourceChat, "Q-20261112-03") {
		op = ytdecOps(t, s, ytwrite.KindDecisionCreate, r.ID)[0]
	}
	s.SetYTDecisionsMode(YTDecisionsOff, "", "", "")
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), op.ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim in mode off: %s %v", out, err)
	}
}

// A structured request's answer (ResolveDecision) writes decision_requests in
// the answer's transaction, so the feed sees it as a "record:<id>" decision
// row, and the snapshot names the answer's channel. A chat answer, which
// never touches refs.decision_request, is journaled the same way.
func TestYTDecisionsResolveEmitsFeedRow(t *testing.T) {
	s := ytdecSetup(t, "ytd_resolve", true)
	task := decisionTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	rec := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceTaskRequest, request.ID), DecisionStatusOpen)
	ytdecPass(t, s)
	creates := ytdecOps(t, s, ytwrite.KindDecisionCreate, rec.ID)
	if len(creates) != 1 {
		t.Fatalf("creates %+v", creates)
	}
	ytdecApplyCreate(t, s, creates[0], ytdecIssue)
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionFields, rec.ID)); n != 1 {
		t.Fatalf("%d on-bind snapshots", n)
	}

	before := ytFeedMaxSyncID(t, s)
	if _, err := s.ResolveDecision(t.Context(), answerA(request, DecisionChannelCLI, "director-1")); err != nil {
		t.Fatal(err)
	}
	var journaled int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM sync_log WHERE model='decision' AND model_id=$1 AND sync_id>$2`,
		DecisionRecordFeedID(rec.ID), before).Scan(&journaled); err != nil || journaled == 0 {
		t.Fatalf("resolve journaled %d rows %v", journaled, err)
	}
	ytdecPass(t, s)
	fields := ytdecOps(t, s, ytwrite.KindDecisionFields, rec.ID)
	if len(fields) != 2 {
		t.Fatalf("fields ops %+v", fields)
	}
	p := ytdecSnapshot(t, fields[1])
	x, ok := ytwrite.ParseDecision(p.Description)
	if !ok || x.Channel != DecisionChannelCLI || x.Answer == "" || x.AnsweredAt == nil || x.TaskID != task.ID || len(x.Options) != 3 ||
		ytdecField(p, ytwrite.FieldDecisionState) != "답변됨" || ytdecField(p, ytwrite.FieldLinkedTask) != strconv.FormatInt(task.ID, 10) {
		t.Fatalf("answered snapshot %+v (parsed %+v %v)", p, x, ok)
	}

	// A chat answer touches no task, and is journaled all the same.
	chat := ytdecChat(t, s, "Q-20261112-04", "chat?")
	mid := ytFeedMaxSyncID(t, s)
	if _, err := s.TransitionChatQuestion(t.Context(), "Q-20261112-04", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	var models []string
	rows, err := s.pool.Query(t.Context(), `SELECT model||':'||model_id FROM sync_log WHERE sync_id>$1 ORDER BY sync_id`, mid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		models = append(models, m)
	}
	rows.Close()
	if strings.Join(models, ",") != "decision:"+DecisionRecordFeedID(chat) {
		t.Fatalf("chat answer journaled %v", models)
	}
}

// The backfill binds a verified issue once, keeps the same binding, and
// refuses one that contradicts a binding or races an unresolved create op;
// in mode on the binding prepares the first snapshot.
func TestYTDecisionsBackfillBinding(t *testing.T) {
	s := ytdecSetup(t, "ytd_bind", true)
	ctx := t.Context()
	now := time.Now().UTC()
	s.SetYTDecisionsMode(YTDecisionsOff, "", "", "")
	a := ytdecChat(t, s, "Q-20261112-05", "a?")
	other := ytdecChat(t, s, "Q-20261112-06", "b?")
	ytdecPass(t, s) // mode off: no create op
	s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, ytdecProjectID, "DEC")

	unbound, err := s.UnboundYTDecisions(ctx, 0, 100)
	if err != nil || len(unbound) != 2 || unbound[0].ID != a || unbound[1].ID != other || unbound[0].PendingOp {
		t.Fatalf("unbound %+v %v", unbound, err)
	}
	bind := func(id int64, issue string) (bool, error) {
		return s.BindYTDecision(ctx, YTDecisionBinding{DecisionID: id, Tenant: ytcTenant, IssueID: issue, IssueReadable: "DEC-" + strconv.FormatInt(id, 10),
			ProjectID: ytdecProjectID, BoundBy: "operator:test"}, now)
	}
	if created, err := bind(a, "2-1"); err != nil || !created {
		t.Fatalf("bind: %v %v", created, err)
	}
	b, _, _ := s.YTDecisionBinding(ctx, a)
	if b.Origin != "import_verified" || b.OpID != 0 || b.IssueReadable != "DEC-"+strconv.FormatInt(a, 10) {
		t.Fatalf("binding %+v", b)
	}
	if fields := ytdecOps(t, s, ytwrite.KindDecisionFields, a); len(fields) != 1 || fields[0].YTTargetID != "2-1" {
		t.Fatalf("first snapshot %+v", fields)
	}
	if created, err := bind(a, "2-1"); err != nil || created {
		t.Fatalf("same binding: %v %v", created, err)
	}
	if n := len(ytdecOps(t, s, ytwrite.KindDecisionFields, a)); n != 1 {
		t.Fatalf("%d snapshots after rebinding the same issue", n)
	}
	if _, err := bind(a, "2-2"); !errors.Is(err, ErrYTDecisionBindingConflict) {
		t.Fatalf("other issue: %v", err)
	}
	if _, err := bind(other, "2-1"); !errors.Is(err, ErrYTDecisionBindingConflict) {
		t.Fatalf("issue of another decision: %v", err)
	}
	if unbound, _ := s.UnboundYTDecisions(ctx, 0, 100); len(unbound) != 1 || unbound[0].ID != other {
		t.Fatalf("unbound after bind %+v", unbound)
	}
	// A bound decision's create is held by the gate, and its changes go to
	// the snapshot.
	if _, err := s.TransitionChatQuestion(ctx, "Q-20261112-05", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	ytdecPass(t, s)
	if c, f := len(ytdecOps(t, s, ytwrite.KindDecisionCreate, a)), len(ytdecOps(t, s, ytwrite.KindDecisionFields, a)); c != 0 || f != 2 {
		t.Fatalf("bound decision: %d creates, %d snapshots", c, f)
	}

	// The other decision gets its create op from the feed: the backfill
	// then refuses to race it.
	if _, err := s.TransitionChatQuestion(ctx, "Q-20261112-06", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	ytdecPass(t, s)
	if unbound, _ := s.UnboundYTDecisions(ctx, 0, 100); len(unbound) != 1 || !unbound[0].PendingOp {
		t.Fatalf("unbound with a create op %+v", unbound)
	}
	if _, err := bind(other, "2-3"); !errors.Is(err, ErrYTDecisionOpPending) {
		t.Fatalf("pending op: %v", err)
	}
}

// A decision_create op whose decision was bound to another issue meanwhile
// applies with binding_conflict and publishes nothing; the guard refuses an
// apply that skips the binding.
func TestYTDecisionsCreateConflictAndGuard(t *testing.T) {
	s := ytdecSetup(t, "ytd_conflict", true)
	ctx := t.Context()
	now := time.Now().UTC()
	id := ytdecChat(t, s, "Q-20261112-07", "c?")
	ytdecPass(t, s)
	op := ytdecOps(t, s, ytwrite.KindDecisionCreate, id)[0]
	// The guard: a resolver that marks the op applied without the binding
	// (the previous release's, which knows no such kind) is refused.
	if _, out, err := s.YTWriteLedger().Claim(ctx, now, op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim: %s %v", out, err)
	}
	oldApply := `UPDATE yt_write_ops SET state='applied', yt_result_id=$2, reason='', last_error='', resolved_at=now() WHERE id=$1`
	if _, err := s.pool.Exec(ctx, oldApply, op.ID, "2-7"); err == nil || !strings.Contains(err.Error(), "applied without its binding") {
		t.Fatalf("guard: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_decision_bindings(decision_id, tenant, issue_id, origin, bound_by, bound_at)
		VALUES ($1,$2,'2-8','import_verified','test',now())`, strconv.FormatInt(id, 10), ytcTenant); err != nil {
		t.Fatal(err)
	}
	got, err := s.YTWriteLedger().Apply(ctx, now, op.ID, "2-7", []ytwrite.MapRow{{EntityKind: "decision", HKKey: op.EntityID, YTID: "2-7", YTParentID: ytdecProjectID, OpID: op.ID}})
	if err != nil || got.State != ytwrite.StateApplied || got.Reason != ytwrite.ReasonBindingConflict {
		t.Fatalf("apply %+v %v", got, err)
	}
	b, _, _ := s.YTDecisionBinding(ctx, id)
	if b.IssueID != "2-8" {
		t.Fatalf("binding overwritten: %+v", b)
	}
	if _, ok, _ := s.YTWriteLedger().MapGet(ctx, "decision", op.EntityID); ok {
		t.Fatal("a conflicting apply wrote the map row")
	}
}

// The feed ids are the "record:" decision rows only.
func TestDecisionFeedIDs(t *testing.T) {
	got := decisionFeedIDs([]FeedRow{{Model: "decision", ModelID: "record:5"}, {Model: "decision", ModelID: "dr-1-1"},
		{Model: "decision", ModelID: "record:05"}, {Model: "task", ModelID: "record:6"}, {Model: "decision", ModelID: "record:7"}})
	if len(got) != 2 || got[0] != 5 || got[1] != 7 {
		t.Fatalf("ids %v", got)
	}
}
