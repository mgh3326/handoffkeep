package ytwrite_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const fieldsTask = int64(4003)

func fieldsTaskRow() ytwrite.TaskFields {
	due := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	return ytwrite.TaskFields{
		TaskID: fieldsTask, Project: "handoffkeep", Title: "ytwrite Y4b — `issue_fields` kind (grade A, T2)",
		TitleKO: "ytwrite Y4b — 필드 종류", BodyDoc: "task-body/4003", PR: "https://github.com/o/r/pull/317", JobID: "hk4003-yt-issue-fields",
		ParentID: 3833, State: "developing", Priority: 2, Lane: "handoffkeep-design", Kind: "implement",
		Stalled: true, DueAt: &due, Area: "youtrack", Milestone: "YouTrack canonical", Expected: 90 * time.Minute,
	}
}

func fieldsReq(issueID string, row ytwrite.TaskFields) ytwrite.FieldsRequest {
	return ytwrite.FieldsRequest{Principal: "hk:task-fields", Actor: "hk:task-fields", Tenant: "tenant.youtrack.cloud",
		IssueID: issueID, ProjectID: issueProject, Task: row}
}

type fieldsEnv struct {
	*kindEnv
	issue string
}

// newFieldsEnv has one bound issue that carries only what issue_create
// wrote, plus a stale 영역 the snapshot must overwrite.
func newFieldsEnv(t *testing.T) fieldsEnv {
	e := fieldsEnv{kindEnv: newKindEnv(t)}
	e.issue = e.yt.InjectIssue(issueProject, fieldsTask, "#4003 old", "old description")
	e.yt.SetIssueField(e.issue, ytwrite.FieldArea, "stale")
	return e
}

func (e fieldsEnv) prepare(row ytwrite.TaskFields, key string) ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.FieldsOp(fieldsReq(e.issue, row))
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

func valueOf(s ytwrite.IssueSnapshot, name string) (string, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func TestFieldsRenderParseRoundTrip(t *testing.T) {
	for name, x := range map[string]ytwrite.FieldsText{
		"full": {TaskID: 4003, Project: "handoffkeep", Title: "ytwrite Y4b (grade A, T2)", TitleKO: "필드 · 종류",
			BodyDoc: "task-body/4003", PR: "https://github.com/o/r/pull/1", JobID: "hk4003", ParentID: 3833},
		"title_only":    {TaskID: 1, Project: "Unassigned", Title: "one"},
		"pr_and_parent": {TaskID: 7, Project: "p", Title: "PR: looks like a line", PR: "#2", ParentID: 9},
		"ko_only":       {TaskID: 8, Project: "p", Title: "t", TitleKO: "Job: not a job"},
	} {
		t.Run(name, func(t *testing.T) {
			text := ytwrite.RenderFields(x)
			got, ok := ytwrite.ParseFields(text)
			if !ok || got != x {
				t.Fatalf("ParseFields(%q) = %+v %v, want %+v", text, got, ok, x)
			}
			if ytwrite.RenderFields(got) != text {
				t.Fatal("render is not stable")
			}
			if strings.Contains(text, "hk-op:") || strings.Contains(text, "작성:") {
				t.Fatalf("description carries an op marker or provenance: %q", text)
			}
		})
	}
	golden := "hk task: #4003 · project: handoffkeep\n제목(원문): t\n본문 문서: task-body/4003\nParent task: #3833"
	if got := ytwrite.RenderFields(ytwrite.FieldsText{TaskID: 4003, Project: "handoffkeep", Title: "t", BodyDoc: "task-body/4003", ParentID: 3833}); got != golden {
		t.Fatalf("rendered %q, want %q", got, golden)
	}
	// Multi-line values are written on their line.
	if got := ytwrite.RenderFields(ytwrite.FieldsText{TaskID: 1, Project: "p", Title: "a\nb  c"}); got != "hk task: #1 · project: p\n제목(원문): a b c" {
		t.Fatalf("multi-line title rendered %q", got)
	}
	for _, bad := range []string{
		"",
		"hk task: #1 · project: p",
		"hk task: #01 · project: p\n제목(원문): t",
		"hk task: #1 · project: p\n제목(원문): t\nPR: x\n제목(한국어): out of order",
		"hk task: #1 · project: p\n제목(원문): t\nPR: x\nPR: twice",
		"hk task: #1 · project: p\n제목(원문): t\nParent task: #0",
		"hk task: #1 · project: p\n제목(원문): t\nPR: ",
		"hk task: #1 · project: p\n제목(원문): t\nsomething else",
		"작성: a · 2026-11-11T00:00:00Z · hk op 1\nhk task: #1 · project: p\n제목(원문): t",
	} {
		if x, ok := ytwrite.ParseFields(bad); ok {
			t.Errorf("ParseFields(%q) = %+v, want refusal", bad, x)
		}
	}
}

func TestTaskIssueSnapshotMapsLikeTheImport(t *testing.T) {
	s := ytwrite.TaskIssueSnapshot(fieldsTaskRow())
	if s.Summary != "#4003 ytwrite Y4b — `issue_fields` kind (grade A, T2)" {
		t.Fatalf("summary %q", s.Summary)
	}
	want := map[string]string{
		"hk ID": "4003", "hk 프로젝트": "handoffkeep", "hk 상태": "개발 중", "hk 우선순위": "2", "레인": "handoffkeep-design",
		"종류": "implement", "등급": "A", "티어": "T2", "PR": "https://github.com/o/r/pull/317", "작업 job": "hk4003-yt-issue-fields",
		"멈춤": "멈춤", "마감": "1795176000000", "영역": "youtrack", "마일스톤": "YouTrack canonical", "예상 소요": "90",
	}
	if len(s.Fields) != len(want) {
		t.Fatalf("fields %+v", s.Fields)
	}
	for name, v := range want {
		if got, ok := valueOf(s, name); !ok || got != v {
			t.Errorf("%s = %q %v, want %q", name, got, ok, v)
		}
	}
	x, ok := ytwrite.ParseFields(s.Description)
	if !ok || x.TaskID != fieldsTask || x.TitleKO != "ytwrite Y4b — 필드 종류" || x.ParentID != 3833 || x.BodyDoc != "task-body/4003" {
		t.Fatalf("description %q parsed %+v %v", s.Description, x, ok)
	}

	// Empty values stay in the list as "" so the update clears them; no
	// project is Unassigned; refs.linear is the grade fallback.
	bare := ytwrite.TaskIssueSnapshot(ytwrite.TaskFields{TaskID: 5, Title: "plain", State: "odd", LinearGrade: "S", LinearTier: "T1"})
	for name, v := range map[string]string{"hk 프로젝트": "Unassigned", "hk 상태": "odd", "등급": "S", "티어": "T1", "멈춤": "", "마감": "", "예상 소요": "", "레인": "", "PR": ""} {
		if got, ok := valueOf(bare, name); !ok || got != v {
			t.Errorf("bare %s = %q %v, want %q", name, got, ok, v)
		}
	}
	if bare.Description != "hk task: #5 · project: Unassigned\n제목(원문): plain" {
		t.Fatalf("bare description %q", bare.Description)
	}
	if ytwrite.TaskIssueSnapshot(fieldsTaskRow()).Description != s.Description {
		t.Fatal("the same task renders differently")
	}
	long := ytwrite.TaskSummary(1, strings.Repeat("가", 300))
	if n := len([]rune(long)); n != ytwrite.IssueSummaryLimit {
		t.Fatalf("summary of %d runes", n)
	}
}

func TestTaskIssueSnapshotRendersKSTDueDate(t *testing.T) {
	for _, due := range []time.Time{
		time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC),
	} {
		row := ytwrite.TaskFields{TaskID: 1, Title: "due", DueAt: &due}
		if got, ok := valueOf(ytwrite.TaskIssueSnapshot(row), ytwrite.FieldDue); !ok || got != "1794657600000" {
			t.Errorf("due %s rendered 마감=%q present=%v, want 1794657600000", due, got, ok)
		}
	}
}

func TestTaskIssueSnapshotRendersSSGrade(t *testing.T) {
	row := fieldsTaskRow()
	row.Title = "top grade task (grade SS, T2)"
	snapshot := ytwrite.TaskIssueSnapshot(row)
	grade, ok := valueOf(snapshot, ytwrite.FieldGrade)
	if !ok || grade != "SS" {
		t.Fatalf("issue_fields grade=%q present=%v, want SS", grade, ok)
	}
	tier, ok := valueOf(snapshot, ytwrite.FieldTier)
	if !ok || tier != "T2" {
		t.Fatalf("issue_fields tier=%q present=%v, want T2", tier, ok)
	}
}

func TestTitleGradeTierCoversStoreScale(t *testing.T) {
	for _, grade := range store.TaskGrades() {
		got, tier := ytwrite.TitleGradeTier("task (grade " + grade + ", T2)")
		if got != grade || tier != "T2" {
			t.Errorf("TitleGradeTier(%q)=(%q,%q)", grade, got, tier)
		}
	}
}

func TestFieldsOpIdentity(t *testing.T) {
	a, err := ytwrite.FieldsOp(fieldsReq("2-1", fieldsTaskRow()))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ytwrite.KindIssueFields || a.EntityKind != "task" || a.EntityID != "4003" || a.YTTargetID != "2-1" || a.Key != "" {
		t.Fatalf("op %+v", a)
	}
	var p ytwrite.IssueFieldsPayload
	if err := json.Unmarshal(a.Payload, &p); err != nil || p.ProjectID != issueProject || len(p.Fields) != 15 {
		t.Fatalf("payload %s: %v", a.Payload, err)
	}
	same, _ := ytwrite.FieldsOp(fieldsReq("2-1", fieldsTaskRow()))
	if same.PayloadSHA != a.PayloadSHA {
		t.Fatal("the same snapshot hashes differently")
	}
	row := fieldsTaskRow()
	row.Priority = 3
	changed, _ := ytwrite.FieldsOp(fieldsReq("2-1", row))
	other, _ := ytwrite.FieldsOp(fieldsReq("2-2", fieldsTaskRow()))
	if changed.PayloadSHA == a.PayloadSHA || other.PayloadSHA == a.PayloadSHA {
		t.Fatal("a changed snapshot or another issue hashes the same")
	}
	for _, bad := range []ytwrite.FieldsRequest{
		fieldsReq("", fieldsTaskRow()),
		{Principal: "hk:task-fields", Actor: "a · b", Tenant: "t", IssueID: "2-1", ProjectID: "0-1", Task: fieldsTaskRow()},
		{Principal: "hk:task-fields", Actor: "a", Tenant: "", IssueID: "2-1", ProjectID: "0-1", Task: fieldsTaskRow()},
		{Principal: "hk:task-fields", Actor: "a", Tenant: "t", IssueID: "2-1", ProjectID: "0-1"},
	} {
		if _, err := ytwrite.FieldsOp(bad); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("FieldsOp(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestIssueFieldsSendSetsEveryFieldAndClears(t *testing.T) {
	e := newFieldsEnv(t)
	row := fieldsTaskRow()
	row.Area, row.Stalled = "", false
	op := e.prepare(row, "k1")
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	if got.YTResultID != e.issue || e.yt.Calls() != 1 || e.yt.Reads() != 0 {
		t.Fatalf("op %+v, calls %d, reads %d", got, e.yt.Calls(), e.yt.Reads())
	}
	ups := e.yt.Updates()
	want := ytwrite.TaskIssueSnapshot(row)
	if len(ups) != 1 || ups[0].IssueID != e.issue || ups[0].ProjectID != issueProject || ytwrite.SnapshotDiff(want, ups[0].IssueSnapshot) != "" || len(ups[0].Fields) != 15 {
		t.Fatalf("updates %+v", ups)
	}
	issue, _ := e.yt.IssueFields(e.issue)
	if d := ytwrite.SnapshotDiff(want, issue); d != "" {
		t.Fatalf("issue differs at %s: %+v", d, issue)
	}
	if v, ok := valueOf(issue, ytwrite.FieldArea); ok {
		t.Fatalf("영역 = %q, want cleared", v)
	}
	if v, _ := valueOf(issue, ytwrite.FieldStalled); v != "" {
		t.Fatalf("멈춤 = %q, want cleared", v)
	}
	if ms, ok, _ := e.ledger.MapGet(context.Background(), "task", "4003"); ok {
		t.Fatalf("an update wrote a map row %+v", ms)
	}
}

func TestIssueFieldsEvidence(t *testing.T) {
	t.Run("equal_applied", func(t *testing.T) {
		e := newFieldsEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		row := fieldsTaskRow()
		due := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
		row.DueAt = &due
		op := e.prepare(row, "k1")
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		if got := e.want(op.ID, ytwrite.StateApplied, ""); got.YTResultID != e.issue || e.yt.Calls() != 1 || e.yt.Reads() != 1 {
			t.Fatalf("op %+v, calls %d, reads %d", got, e.yt.Calls(), e.yt.Reads())
		}
	})
	t.Run("one_field_differs_unknown", func(t *testing.T) {
		e := newFieldsEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := e.prepare(fieldsTaskRow(), "k1")
		e.drive()
		e.yt.SetIssueField(e.issue, ytwrite.FieldLane, "someone-else")
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		if e.yt.Calls() != 1 {
			t.Fatalf("resent: %d calls", e.yt.Calls())
		}
	})
	t.Run("different_due_day_unknown", func(t *testing.T) {
		e := newFieldsEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := e.prepare(fieldsTaskRow(), "k1")
		e.drive()
		e.yt.SetIssueField(e.issue, ytwrite.FieldDue, "1794744000000")
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
	})
	t.Run("nil_due_matches_null", func(t *testing.T) {
		e := newFieldsEnv(t)
		row := fieldsTaskRow()
		row.DueAt = nil
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := e.prepare(row, "k1")
		e.drive()
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateApplied, "")
	})
	t.Run("not_written_unknown", func(t *testing.T) {
		e := newFieldsEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.ServerError})
		op := e.prepare(fieldsTaskRow(), "k1")
		e.drive()
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		if e.yt.Calls() != 1 {
			t.Fatalf("resent: %d calls", e.yt.Calls())
		}
	})
	t.Run("read_429_waits", func(t *testing.T) {
		e := newFieldsEnv(t)
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op := e.prepare(fieldsTaskRow(), "k1")
		e.drive()
		e.yt.QueueRead(&ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Hour})
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonReconcileFailed)
		if got.NextAttemptAt.Before(e.clock.Now().Add(2 * time.Hour)) {
			t.Fatalf("next search at %v, before the Retry-After", got.NextAttemptAt)
		}
	})
}

func TestIssueFieldsMissingBundleValueRejected(t *testing.T) {
	e := newFieldsEnv(t)
	e.yt.SetBundle(ytwrite.FieldLane, "other-lane")
	op := e.prepare(fieldsTaskRow(), "k1")
	e.drive()
	got := e.want(op.ID, ytwrite.StateRejected, "http_400")
	if !strings.Contains(got.LastError, "레인") || !strings.Contains(got.LastError, "handoffkeep-design") {
		t.Fatalf("last_error %q does not name the field and value", got.LastError)
	}
}

// Ops queue on the task's lane: the second snapshot goes out after the
// first, so YouTrack ends at the latest.
func TestIssueFieldsOrderedOnTheTaskLane(t *testing.T) {
	e := newFieldsEnv(t)
	first := e.prepare(fieldsTaskRow(), "k1")
	row := fieldsTaskRow()
	row.State = "reviewing"
	second := e.prepare(row, "k2")
	e.drive()
	e.want(first.ID, ytwrite.StateApplied, "")
	e.want(second.ID, ytwrite.StateApplied, "")
	if issue, _ := e.yt.IssueFields(e.issue); func() string { v, _ := valueOf(issue, ytwrite.FieldState); return v }() != "검토 중" {
		t.Fatalf("issue ended at %+v", issue)
	}
}

func TestIssueFieldsNeedsAFieldsRemote(t *testing.T) {
	e := newFieldsEnv(t)
	p, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: commentsOnly{e.yt}, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	op := e.prepare(fieldsTaskRow(), "k1")
	if _, err := p.Drive(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind)
	if e.yt.Calls() != 0 {
		t.Fatal("sent through a remote without the fields surface")
	}
}
