package ytwrite_test

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const (
	decProject  = "0-12"
	decisionID  = int64(4054)
	decTenant   = "tenant.youtrack.cloud"
	decActor    = "hk:decisions"
	longRepeats = 40
)

func answeredAt() *time.Time {
	at := time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)
	return &at
}

// decisionTexts are the round-trip cases the brief names: Korean, a question
// over 250 runes, 0 and 5 options, unanswered and answered.
func decisionTexts() map[string]ytwrite.DecisionText {
	long := strings.TrimSpace(strings.Repeat("결정 이슈의 질문 전문은 250자를 넘어도 잘리지 않는다. ", longRepeats))
	five := []ytwrite.DecisionOption{{Key: "A", Label: "권고대로 진행"}, {Key: "B", Label: "보류"}, {Key: "C", Label: "다른 안"},
		{Key: "D", Label: ""}, {Key: "", Label: "키 없음"}}
	return map[string]ytwrite.DecisionText{
		"korean_unanswered_no_options": {DecisionID: decisionID, TaskID: 3833, Question: "YouTrack을 정본으로 할까요?\n\n> 인용처럼 보이는 줄\n- 목록"},
		"long_five_options_answered": {DecisionID: decisionID, TaskID: 3833, Question: long + "\n둘째 줄", Options: five,
			Recommended: "A", DefaultAction: "권장안으로 진행", Answer: "A로 진행\n\n이유: 빠르다", AnsweredBy: "operator",
			Channel: "console", AnsweredAt: answeredAt()},
		"no_task_answered_no_channel": {DecisionID: 7, Question: "q", Answer: "yes", AnsweredBy: "robin", AnsweredAt: answeredAt()},
		"empty_question":              {DecisionID: 8},
		"block_shaped_question":       {DecisionID: 9, Question: "답:\n권장: X\n1. [A] b\n>", Options: []ytwrite.DecisionOption{{Key: "A", Label: "b"}}},
	}
}

func TestDecisionRenderParseRoundTrip(t *testing.T) {
	for name, x := range decisionTexts() {
		t.Run(name, func(t *testing.T) {
			if c := ytwrite.CanonicalDecision(x); !reflect.DeepEqual(c, x) {
				t.Fatalf("test input is not canonical:\n%+v\n%+v", c, x)
			}
			block := ytwrite.RenderDecision(x)
			got, ok := ytwrite.ParseDecision(block)
			if !ok || !reflect.DeepEqual(got, x) {
				t.Fatalf("Parse(Render(x)) = %+v, %v\nwant %+v\nblock:\n%s", got, ok, x, block)
			}
			if again := ytwrite.RenderDecision(got); again != block {
				t.Fatalf("render is not stable:\n%s\n---\n%s", block, again)
			}
		})
	}
}

// The full question and the options reach the description even when the
// summary is cut at 250 runes.
func TestDecisionSnapshotKeepsTheFullText(t *testing.T) {
	x := decisionTexts()["long_five_options_answered"]
	due := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	snap := ytwrite.DecisionIssueSnapshot(ytwrite.DecisionFields{DecisionText: x, Status: "answered", Lane: "handoffkeep-design", DueAt: &due})
	if n := utf8.RuneCountInString(snap.Summary); n != ytwrite.IssueSummaryLimit || !strings.HasPrefix(snap.Summary, "D-4054 결정") {
		t.Fatalf("summary %q (%d runes)", snap.Summary, n)
	}
	if utf8.RuneCountInString(x.Question) <= ytwrite.IssueSummaryLimit {
		t.Fatal("the long question is not over the summary limit")
	}
	for _, want := range []string{"> 둘째 줄", "5. [] 키 없음", "4. [D]", "권장: A", "기본 동작: 권장안으로 진행", "> 이유: 빠르다", "답한 사람: operator",
		"답한 경로: console", "답한 시각: 2026-11-12T03:04:05Z", "연결 작업: #3833"} {
		if !strings.Contains(snap.Description, want) {
			t.Fatalf("description lacks %q:\n%s", want, snap.Description)
		}
	}
	if !strings.Contains(snap.Description, strings.TrimSpace(strings.Repeat("결정 이슈의 질문 전문은 250자를 넘어도 잘리지 않는다. ", longRepeats))) {
		t.Fatal("description does not carry the full question")
	}
	want := map[string]string{ytwrite.FieldHKID: "4054", ytwrite.FieldDecisionID: "4054", ytwrite.FieldDecisionState: "답변됨",
		ytwrite.FieldLane: "handoffkeep-design", ytwrite.FieldRecommended: "A", ytwrite.FieldDecisionDue: strconv.FormatInt(due.UnixMilli(), 10),
		ytwrite.FieldLinkedTask: "3833"}
	if len(snap.Fields) != len(want) {
		t.Fatalf("fields %+v", snap.Fields)
	}
	for _, f := range snap.Fields {
		if want[f.Name] != f.Value {
			t.Fatalf("field %s = %q, want %q", f.Name, f.Value, want[f.Name])
		}
	}
	open := ytwrite.DecisionIssueSnapshot(ytwrite.DecisionFields{DecisionText: ytwrite.DecisionText{DecisionID: 3, Question: "q"}, Status: "open"})
	for _, f := range open.Fields {
		if (f.Name == ytwrite.FieldLinkedTask || f.Name == ytwrite.FieldDecisionDue || f.Name == ytwrite.FieldRecommended) && f.Value != "" {
			t.Fatalf("empty field %s = %q", f.Name, f.Value)
		}
		if f.Name == ytwrite.FieldDecisionState && f.Value != "열림" {
			t.Fatalf("state %q", f.Value)
		}
	}
}

func TestParseDecisionRefusesOtherText(t *testing.T) {
	good := ytwrite.RenderDecision(decisionTexts()["long_five_options_answered"])
	for name, text := range map[string]string{
		"empty":             "",
		"no_head":           strings.TrimPrefix(good, "hk decision: "),
		"trailing_space":    strings.Replace(good, "> 둘째 줄", "> 둘째 줄 ", 1),
		"extra_line":        good + "\nmore",
		"option_gap":        strings.Replace(good, "2. [B]", "3. [B]", 1),
		"bad_time":          strings.Replace(good, "2026-11-12T03:04:05Z", "2026-11-12 03:04:05", 1),
		"answer_after_who":  strings.Replace(ytwrite.RenderDecision(ytwrite.DecisionText{DecisionID: 1, AnsweredBy: "x"}), "답한 사람: x", "답한 사람: x\n답:\n> y", 1),
		"leading_zero_head": strings.Replace(good, "D-4054", "D-04054", 1),
	} {
		if _, ok := ytwrite.ParseDecision(text); ok {
			t.Fatalf("%s: parsed", name)
		}
	}
}

func TestSummaryNamesDecision(t *testing.T) {
	for summary, want := range map[string]bool{"D-37 q": true, "D-37": true, "D-3 q": false, "D-370 q": false, "#37 q": false, "D-37\tq": true} {
		if got := ytwrite.SummaryNamesDecision(summary, 37); got != want {
			t.Fatalf("SummaryNamesDecision(%q) = %v", summary, got)
		}
	}
}

type decisionEnv struct{ *kindEnv }

func newDecisionEnv(t *testing.T) decisionEnv { return decisionEnv{newKindEnv(t)} }

func decisionReq() ytwrite.DecisionRequest {
	return ytwrite.DecisionRequest{Principal: decActor, Actor: decActor, YTProjectID: decProject, YTProjectShort: "DEC", Tenant: decTenant,
		Decision: decisionTexts()["long_five_options_answered"]}
}

func (e decisionEnv) prepareCreate() ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.DecisionOp(decisionReq())
	if err != nil {
		e.t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func (e decisionEnv) wantMap(op ytwrite.Op, issueID string, adopted bool) {
	e.t.Helper()
	m, ok, err := e.ledger.MapGet(context.Background(), ytwrite.DecisionEntity, "4054")
	if err != nil || !ok || m.YTID != issueID || m.YTParentID != decProject || m.OpID != op.ID || m.Adopted != adopted {
		e.t.Fatalf("decision map row %+v %v %v, want issue %s op %d adopted %v", m, ok, err, issueID, op.ID, adopted)
	}
}

func TestDecisionCreateCreatesOneWithTheFullText(t *testing.T) {
	e := newDecisionEnv(t)
	op := e.prepareCreate()
	if op.Key != "decision-issue:4054" || op.EntityKind != "decision" || op.EntityID != "4054" || op.YTTargetID != decProject {
		t.Fatalf("op %+v", op)
	}
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	issues := e.yt.Issues(decProject)
	creates := e.yt.DecisionCreates()
	if len(issues) != 1 || issues[0].ID != got.YTResultID || len(creates) != 1 || e.yt.Finds() != 1 {
		t.Fatalf("issues %+v creates %+v finds %d", issues, creates, e.yt.Finds())
	}
	x := decisionReq().Decision
	wantDesc := ytwrite.RenderDecisionCreate(decActor, op.CreatedAt, op.ID, x)
	if creates[0].Summary != ytwrite.DecisionSummary(decisionID, x.Question) || creates[0].Description != wantDesc || creates[0].DecisionID != decisionID {
		t.Fatalf("create %+v", creates[0])
	}
	block := strings.TrimSuffix(strings.SplitN(wantDesc, "\n", 2)[1], "\n\n"+ytwrite.OpMarker(op.ID))
	if parsed, ok := ytwrite.ParseDecision(block); !ok || !reflect.DeepEqual(parsed, x) {
		t.Fatalf("create description block does not round-trip: %v", ok)
	}
	e.wantMap(got, got.YTResultID, false)
}

func TestDecisionCreateAdoptsTheImportedIssue(t *testing.T) {
	e := newDecisionEnv(t)
	imported := e.yt.InjectDecisionIssue(decProject, decisionID, "D-4054 질문 앞부분", "답변: A")
	e.yt.InjectDecisionIssue("0-99", decisionID, "D-4054 another project", "")
	op := e.prepareCreate()
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	if got.YTResultID != imported || len(e.yt.DecisionCreates()) != 0 || e.yt.Calls() != 0 {
		t.Fatalf("op %+v creates %d", got, len(e.yt.DecisionCreates()))
	}
	e.wantMap(got, imported, true)
}

func TestDecisionCreateAmbiguousSendsNothing(t *testing.T) {
	for name, inject := range map[string]func(*ytwritetest.FakeYouTrack){
		"two": func(yt *ytwritetest.FakeYouTrack) {
			yt.InjectDecisionIssue(decProject, decisionID, "D-4054 a", "")
			yt.InjectDecisionIssue(decProject, decisionID, "D-4054 b", "")
		},
		"not_named": func(yt *ytwritetest.FakeYouTrack) {
			yt.InjectDecisionIssue(decProject, decisionID, "[결정 4054] legacy", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDecisionEnv(t)
			inject(e.yt)
			op := e.prepareCreate()
			e.drive()
			e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonAmbiguousIssue)
			if e.yt.Calls() != 0 {
				t.Fatalf("%d creates", e.yt.Calls())
			}
		})
	}
}

func TestDecisionCreateHeldWhileFieldsMissing(t *testing.T) {
	e := newDecisionEnv(t)
	e.yt.SetIssueFieldsMissing(decProject, true)
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	if e.yt.Calls() != 0 {
		t.Fatalf("%d creates", e.yt.Calls())
	}
}

// A lost reply leaves the create remote_unknown; the exact description is
// the evidence that applies it, with no second POST.
func TestDecisionCreateLostReplyEvidenceApplies(t *testing.T) {
	e := newDecisionEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	issues := e.yt.Issues(decProject)
	if len(issues) != 1 || got.YTResultID != issues[0].ID || e.yt.Calls() != 1 {
		t.Fatalf("op %+v issues %+v creates %d", got, issues, e.yt.Calls())
	}
	e.wantMap(got, issues[0].ID, false)
}

// A 5xx whose create did not commit, then an issue that only quotes the
// marker: no exact match, the op stays remote_unknown and nothing is resent.
func TestDecisionCreateLostReplyWithoutEvidenceStaysUnknown(t *testing.T) {
	e := newDecisionEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusInternalServerError})
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
	e.yt.InjectDecisionIssue(decProject, decisionID, "D-4054 copy", "copied\n\n"+ytwrite.OpMarker(op.ID))
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
	if e.yt.Calls() != 1 {
		t.Fatalf("%d creates", e.yt.Calls())
	}
}

func TestDecisionCreateRefusesTheMarker(t *testing.T) {
	req := decisionReq()
	req.Decision.Answer = "quoting " + ytwrite.OpMarker(1)
	if err := ytwrite.CheckDecision(req); err == nil {
		t.Fatal("a decision quoting the marker was accepted")
	}
	if err := ytwrite.CheckDecision(decisionReq()); err != nil {
		t.Fatal(err)
	}
}

func (e decisionEnv) prepareFields(issueID, key string, d ytwrite.DecisionFields) ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.DecisionFieldsOp(ytwrite.DecisionFieldsRequest{Principal: decActor, Actor: decActor, Tenant: decTenant,
		IssueID: issueID, ProjectID: decProject, Decision: d})
	if err != nil {
		e.t.Fatal(err)
	}
	n.Key = key
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func TestDecisionFieldsWritesTheSnapshotAndEvidence(t *testing.T) {
	e := newDecisionEnv(t)
	issue := e.yt.InjectDecisionIssue(decProject, decisionID, "D-4054 old", "답변: A")
	d := ytwrite.DecisionFields{DecisionText: decisionReq().Decision, Status: "answered", Lane: "handoffkeep-design"}
	op := e.prepareFields(issue, "decision_fields:4054:1:a", d)
	if op.EntityKind != "decision" || op.YTTargetID != issue {
		t.Fatalf("op %+v", op)
	}
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	got, _ := e.yt.IssueFields(issue)
	if diff := ytwrite.SnapshotDiff(ytwrite.DecisionIssueSnapshot(d), got); diff != "" {
		t.Fatalf("issue differs in %s: %+v", diff, got)
	}

	// Lost reply: the GET that compares the snapshot applies it.
	d.Status, d.Lane = "superseded", "other-lane"
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	lost := e.prepareFields(issue, "decision_fields:4054:2:b", d)
	e.drive()
	e.want(lost.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(lost.ID, ytwrite.StateApplied, "")

	// A lost reply whose write did not land, and a person's edit: unknown.
	d.Status = "open"
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	miss := e.prepareFields(issue, "decision_fields:4054:3:c", d)
	e.drive()
	e.want(miss.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(miss.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
}

// A decision_fields op is a decision lane's: one on a task lane does not
// render, so it is rejected before anything is sent.
func TestDecisionFieldsRefusesATaskLane(t *testing.T) {
	e := newDecisionEnv(t)
	issue := e.yt.InjectDecisionIssue(decProject, decisionID, "D-4054 old", "")
	n, err := ytwrite.DecisionFieldsOp(ytwrite.DecisionFieldsRequest{Principal: decActor, Actor: decActor, Tenant: decTenant,
		IssueID: issue, ProjectID: decProject, Decision: ytwrite.DecisionFields{DecisionText: ytwrite.DecisionText{DecisionID: decisionID, Question: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	n.Key, n.EntityKind = "k", "task"
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind)
	if e.yt.Calls() != 0 {
		t.Fatalf("%d sends", e.yt.Calls())
	}
}
