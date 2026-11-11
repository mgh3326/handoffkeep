package ytwrite_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const (
	issueProject = "0-7"
	issueTask    = int64(3898)
)

func issueReq() ytwrite.IssueRequest {
	return ytwrite.IssueRequest{Principal: "hk:task-issues", Actor: "handoffkeep-design", TaskID: issueTask,
		Title: "ytwrite Y4a — `issue_create` kind (grade S, T2)", Project: "handoffkeep",
		YTProjectID: issueProject, YTProjectShort: "HK", Tenant: "tenant.youtrack.cloud"}
}

type issueEnv struct {
	*kindEnv
}

func newIssueEnv(t *testing.T) issueEnv { return issueEnv{newKindEnv(t)} }

// prepareIssue stores the task's issue_create op as the store does.
func (e issueEnv) prepareIssue() ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.IssueOp(issueReq())
	if err != nil {
		e.t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func (e issueEnv) rendered(op ytwrite.Op) (summary, description string) {
	e.t.Helper()
	x, ok := ytwrite.ParseIssue(mustRender(e.t, e.p, op))
	if !ok {
		e.t.Fatal("rendered description does not parse")
	}
	return ytwrite.RenderIssue(x)
}

func mustRender(t *testing.T, p *ytwrite.Protocol, op ytwrite.Op) string {
	t.Helper()
	text, err := p.Kinds[ytwrite.KindIssueCreate].Render(op)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func (e issueEnv) wantMap(op ytwrite.Op, issueID string) {
	e.t.Helper()
	m, ok, err := e.ledger.MapGet(context.Background(), "task", "3898")
	if err != nil || !ok || m.YTID != issueID || m.YTParentID != issueProject || m.OpID != op.ID {
		e.t.Fatalf("task map row %+v %v %v, want issue %s parent %s op %d", m, ok, err, issueID, issueProject, op.ID)
	}
	if _, ok, _ := e.ledger.MapGet(context.Background(), "comment", "op:"+strconv.FormatInt(op.ID, 10)); ok {
		e.t.Fatal("an issue op wrote a comment map row")
	}
}

func TestIssueCreateNoneCreatesOne(t *testing.T) {
	e := newIssueEnv(t)
	op := e.prepareIssue()
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	issues := e.yt.Issues(issueProject)
	if len(issues) != 1 || issues[0].ID != got.YTResultID || e.yt.Calls() != 1 || e.yt.Finds() != 1 {
		t.Fatalf("issues %+v, op %+v, creates %d, finds %d", issues, got, e.yt.Calls(), e.yt.Finds())
	}
	summary, description := e.rendered(got)
	if issues[0].Summary != summary || issues[0].Description != description {
		t.Fatalf("issue %+v, want summary %q description %q", issues[0], summary, description)
	}
	if !strings.HasPrefix(summary, "#3898 ") || !ytwrite.SummaryNamesTask(summary, issueTask) || !strings.HasSuffix(description, ytwrite.OpMarker(op.ID)) {
		t.Fatalf("summary %q description %q", summary, description)
	}
	e.wantMap(got, got.YTResultID)
	if m, _, _ := e.ledger.MapGet(context.Background(), "task", "3898"); m.Adopted {
		t.Fatalf("map row %+v of a created issue marked adopted", m)
	}
}

func TestIssueCreateAdoptsTheOneNamedIssue(t *testing.T) {
	e := newIssueEnv(t)
	existing := e.yt.InjectIssue(issueProject, issueTask, "#3898 imported earlier", "legacy body")
	e.yt.InjectIssue("0-9", issueTask, "#3898 another project", "") // another project's hk ID is not this task's issue
	op := e.prepareIssue()
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	if got.YTResultID != existing || e.yt.Calls() != 0 || len(e.yt.Issues(issueProject)) != 1 {
		t.Fatalf("op %+v, creates %d, issues %+v", got, e.yt.Calls(), e.yt.Issues(issueProject))
	}
	e.wantMap(got, existing)
	if m, _, _ := e.ledger.MapGet(context.Background(), "task", "3898"); !m.Adopted {
		t.Fatalf("map row %+v not marked adopted", m)
	}
}

func TestIssueCreateAmbiguousSendsNothing(t *testing.T) {
	for name, inject := range map[string]func(*ytwritetest.FakeYouTrack){
		"two_issues": func(yt *ytwritetest.FakeYouTrack) {
			yt.InjectIssue(issueProject, issueTask, "#3898 one", "")
			yt.InjectIssue(issueProject, issueTask, "#3898 two", "")
		},
		"one_issue_not_named": func(yt *ytwritetest.FakeYouTrack) {
			yt.InjectIssue(issueProject, issueTask, "#38980 someone else", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newIssueEnv(t)
			inject(e.yt)
			before := len(e.yt.Issues(issueProject))
			op := e.prepareIssue()
			e.drive()
			got := e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonAmbiguousIssue)
			if e.yt.Calls() != 0 || len(e.yt.Issues(issueProject)) != before || got.Attempts != 1 {
				t.Fatalf("op %+v, creates %d, issues %d", got, e.yt.Calls(), len(e.yt.Issues(issueProject)))
			}
			if _, ok, _ := e.ledger.MapGet(context.Background(), "task", "3898"); ok {
				t.Fatal("a rejected op wrote a map row")
			}
		})
	}
}

// A create whose reply was lost is never sent again: the one issue carrying
// the marker is the evidence that applies it.
func TestIssueCreateLostReplyEvidenceApplies(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	op := e.prepareIssue()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	issues := e.yt.Issues(issueProject)
	if len(issues) != 1 || got.YTResultID != issues[0].ID || e.yt.Calls() != 1 || got.Reconciles != 0 {
		t.Fatalf("op %+v, issues %+v, creates %d", got, issues, e.yt.Calls())
	}
	e.wantMap(got, issues[0].ID)
}

// Evidence needs the exact rendered description: a 5xx create followed by
// an unrelated issue that quotes the op's marker is not the op's issue, and
// an edited description is not proof either; both stay remote_unknown. The
// exact text, appearing late, applies.
func TestIssueCreateEvidenceNeedsTheExactText(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusInternalServerError})
	op := e.prepareIssue()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	e.yt.InjectIssue(issueProject, issueTask, "#3898 unrelated", "copied from elsewhere\n\n"+ytwrite.OpMarker(op.ID))
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)

	f := newIssueEnv(t)
	f.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusInternalServerError})
	op = f.prepareIssue()
	f.drive()
	summary, description := f.rendered(op)
	id := f.yt.InjectIssue(issueProject, issueTask, summary, description)
	f.clock.Advance(time.Hour)
	f.drive()
	if got := f.want(op.ID, ytwrite.StateApplied, ""); got.YTResultID != id {
		t.Fatalf("op %+v, want %s", got, id)
	}
}

// A text hk project field (the importer's shape) is sent as text.
func TestIssueCreateSendsTheProjectFieldShape(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.SetProjectFieldText(issueProject)
	op := e.prepareIssue()
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if c := e.yt.Created(); len(c) != 1 || !c[0].HKProjectText || c[0].HKProject != "handoffkeep" || c[0].TaskID != issueTask {
		t.Fatalf("creates %+v", c)
	}
}

func TestIssueCreateEvidenceOtherwiseUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		seed   func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string)
	}{
		{"zero_issues", ytwrite.ReasonNoEvidence, func(*ytwritetest.FakeYouTrack, ytwrite.Op, string) {}},
		{"issue_without_marker", ytwrite.ReasonMarkerMismatch, func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string) {
			yt.InjectIssue(issueProject, issueTask, "#3898 made by hand", "no marker")
		}},
		{"another_ops_marker", ytwrite.ReasonMarkerMismatch, func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string) {
			yt.InjectIssue(issueProject, issueTask, "#3898", ytwrite.OpMarker(op.ID*10))
		}},
		{"quoted_marker", ytwrite.ReasonMarkerMismatch, func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string) {
			yt.InjectIssue(issueProject, issueTask, "#3898", "quote "+ytwrite.OpMarker(op.ID))
		}},
		{"two_exact_hits", ytwrite.ReasonMultipleMatches, func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string) {
			yt.InjectIssue(issueProject, issueTask, "#3898 a", desc)
			yt.InjectIssue(issueProject, issueTask, "#3898 b", desc)
		}},
		{"exact_hit_and_another_issue", ytwrite.ReasonMarkerMismatch, func(yt *ytwritetest.FakeYouTrack, op ytwrite.Op, desc string) {
			yt.InjectIssue(issueProject, issueTask, "#3898 a", desc)
			yt.InjectIssue(issueProject, issueTask, "#3898 b", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIssueEnv(t)
			e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
			op := e.prepareIssue()
			e.drive()
			_, desc := e.rendered(op)
			tc.seed(e.yt, op, desc)
			for range 3 {
				e.clock.Advance(time.Hour)
				e.drive()
			}
			got := e.want(op.ID, ytwrite.StateRemoteUnknown, tc.reason)
			if got.Reconciles != 3 || e.yt.Calls() != 1 {
				t.Fatalf("op %+v, creates %d: an unknown issue op is never re-created", got, e.yt.Calls())
			}
		})
	}
}

func TestIssueCreate4xxRejected(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadRequest})
	op := e.prepareIssue()
	e.drive()
	if got := e.want(op.ID, ytwrite.StateRejected, "http_400"); e.yt.Calls() != 1 || len(e.yt.Issues(issueProject)) != 0 {
		t.Fatalf("op %+v, creates %d", got, e.yt.Calls())
	}
}

// A search that fails before the create sent nothing: the op is prepared
// again (never remote_unknown), and a 4xx search is final.
func TestIssueCreateFindFailureSendsNothing(t *testing.T) {
	t.Run("transport_then_created", func(t *testing.T) {
		e := newIssueEnv(t)
		e.yt.QueueFind(errors.New("connection reset"))
		op := e.prepareIssue()
		e.drive()
		e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
		e.clock.Advance(time.Hour)
		e.drive()
		if got := e.want(op.ID, ytwrite.StateApplied, ""); e.yt.Calls() != 1 || got.Attempts != 2 {
			t.Fatalf("op %+v, creates %d", got, e.yt.Calls())
		}
	})
	t.Run("rate_limited", func(t *testing.T) {
		e := newIssueEnv(t)
		e.yt.QueueFind(&ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: 90 * time.Second})
		op := e.prepareIssue()
		e.drive()
		got := e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		if want := e.clock.Now().Add(90 * time.Second); !got.NextAttemptAt.Equal(want) || e.yt.Calls() != 0 {
			t.Fatalf("op %+v, want next attempt %v", got, want)
		}
	})
	t.Run("server_error", func(t *testing.T) {
		e := newIssueEnv(t)
		e.yt.QueueFind(&ytwrite.RemoteError{Status: http.StatusServiceUnavailable})
		op := e.prepareIssue()
		e.drive()
		e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	})
	t.Run("4xx", func(t *testing.T) {
		e := newIssueEnv(t)
		e.yt.QueueFind(&ytwrite.RemoteError{Status: http.StatusBadRequest, Message: "unknown field"})
		op := e.prepareIssue()
		e.drive()
		if e.want(op.ID, ytwrite.StateRejected, "http_400"); e.yt.Calls() != 0 {
			t.Fatalf("creates %d", e.yt.Calls())
		}
	})
}

// commentsOnly is a Remote that cannot create issues.
type commentsOnly struct{ ytwrite.Remote }

func TestIssueCreateNeedsAnIssueRemote(t *testing.T) {
	e := newIssueEnv(t)
	p, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: commentsOnly{e.yt}, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.p = p
	op := e.prepareIssue()
	e.drive()
	if e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind); e.yt.Calls() != 0 || e.yt.Finds() != 0 {
		t.Fatalf("creates %d finds %d", e.yt.Calls(), e.yt.Finds())
	}
}

// An operator's resolve applies through the kind's map rows, also on a
// Protocol built without New (the API's).
func TestIssueCreateResolveWritesTaskMapRow(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	op := e.prepareIssue()
	e.drive()
	bare := &ytwrite.Protocol{Config: ytwrite.Config{Ledger: e.ledger, Now: e.clock.Now}}
	res, err := bare.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: "2-77", By: "operator"})
	if err != nil || res.Status != ytwrite.StatusApplied || res.YTID != "2-77" {
		t.Fatalf("resolve %+v %v", res, err)
	}
	e.wantMap(e.want(op.ID, ytwrite.StateApplied, ""), "2-77")
	// By default the operator says the op created the issue.
	if m, _, _ := e.ledger.MapGet(context.Background(), "task", "3898"); m.Adopted {
		t.Fatalf("resolved map row %+v marked adopted", m)
	}
}

// --adopted marks the resolution as an adoption; adopted without applied is
// refused before anything is written.
func TestIssueCreateResolveAdopted(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	op := e.prepareIssue()
	e.drive()
	bare := &ytwrite.Protocol{Config: ytwrite.Config{Ledger: e.ledger, Now: e.clock.Now}}
	if _, err := bare.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: false, Adopted: true, By: "operator", Note: "n"}); !errors.Is(err, ytwrite.ErrInvalid) {
		t.Fatalf("adopted without applied: %v", err)
	}
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	if _, err := bare.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: "2-78", Adopted: true, By: "operator"}); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := e.ledger.MapGet(context.Background(), "task", "3898"); !m.Adopted || m.YTID != "2-78" {
		t.Fatalf("map row %+v, want adopted 2-78", m)
	}
}

func TestIssueOpIdentity(t *testing.T) {
	n, err := ytwrite.IssueOp(issueReq())
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != ytwrite.KindIssueCreate || n.Key != "task-issue:3898" || n.EntityKind != "task" || n.EntityID != "3898" ||
		n.YTTargetID != issueProject || n.Tenant != "tenant.youtrack.cloud" || n.Actor != "handoffkeep-design" {
		t.Fatalf("op %+v", n)
	}
	other := issueReq()
	other.Title += " (renamed)"
	m, _ := ytwrite.IssueOp(other)
	if m.PayloadSHA == n.PayloadSHA || m.Key != n.Key {
		t.Fatal("a renamed task must be another payload under the same key")
	}
	if err := ytwrite.CheckIssue(issueReq()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*ytwrite.IssueRequest){
		"marker in title":    func(r *ytwrite.IssueRequest) { r.Title = "quote <!-- hk-op:1 -->" },
		"empty title":        func(r *ytwrite.IssueRequest) { r.Title = " " },
		"separator in actor": func(r *ytwrite.IssueRequest) { r.Actor = "a · b" },
		"unmapped project":   func(r *ytwrite.IssueRequest) { r.YTProjectID = "" },
		"no tenant":          func(r *ytwrite.IssueRequest) { r.Tenant = "" },
		"multi-line project": func(r *ytwrite.IssueRequest) { r.Project = "a\nb" },
	} {
		r := issueReq()
		mut(&r)
		if err := ytwrite.CheckIssue(r); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestIssueRenderParseRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 11, 5, 6, 7, 0, time.UTC)
	for _, x := range []ytwrite.IssueText{
		{Actor: "handoffkeep-design", At: at, OpID: 41, TaskID: 3898, Project: "handoffkeep", Title: "ytwrite Y4a — `issue_create` (grade S, T2)"},
		{Actor: "operator:me@example.com", At: at, OpID: 1, TaskID: 7, Project: "", Title: "여러 줄\n제목\n\n끝 · end"},
		{Actor: "x", At: at, OpID: 9, TaskID: 12, Project: "p", Title: strings.Repeat("긴 제목 ", 100)},
	} {
		summary, description := ytwrite.RenderIssue(x)
		got, ok := ytwrite.ParseIssue(description)
		if !ok || got != x {
			t.Fatalf("Parse(Render(%+v)) = %+v %v\n%s", x, got, ok, description)
		}
		if strings.ContainsAny(summary, "\n") || len([]rune(summary)) > ytwrite.IssueSummaryLimit || !ytwrite.SummaryNamesTask(summary, x.TaskID) {
			t.Fatalf("summary %q", summary)
		}
	}
	want := "작성: handoffkeep-design · 2026-11-11T05:06:07Z · hk op 41\nhk task: #3898 · project: handoffkeep\n제목(원문): t\n\n<!-- hk-op:41 -->"
	if _, d := ytwrite.RenderIssue(ytwrite.IssueText{Actor: "handoffkeep-design", At: at, OpID: 41, TaskID: 3898, Project: "handoffkeep", Title: "t"}); d != want {
		t.Fatalf("description %q, want %q", d, want)
	}
	for _, bad := range []string{"", "작성: a · 2026-11-11T05:06:07Z · hk op 1", want[:len(want)-4], strings.Replace(want, "#3898", "#03898", 1), strings.Replace(want, "hk-op:41", "hk-op:42", 1)} {
		if _, ok := ytwrite.ParseIssue(bad); ok {
			t.Errorf("ParseIssue(%q) ok", bad)
		}
	}
}

func TestSummaryNamesTask(t *testing.T) {
	for summary, want := range map[string]bool{"#37": true, "#37 title": true, "#37\ttitle": true, "#3739 x": false, "#037 x": false, "37 x": false, "#3 x": false} {
		if got := ytwrite.SummaryNamesTask(summary, 37); got != want {
			t.Errorf("SummaryNamesTask(%q, 37) = %v", summary, got)
		}
	}
}

// A project without the issue fields holds the op before anything is
// searched or sent; once the schema is repaired the op goes out.
func TestIssueCreateHeldUntilFieldsExist(t *testing.T) {
	e := newIssueEnv(t)
	e.yt.SetIssueFieldsMissing(issueProject, true)
	op := e.prepareIssue()
	for range 3 {
		e.drive()
		e.clock.Advance(time.Hour)
	}
	got := e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	if e.yt.Finds() != 0 || e.yt.Calls() != 0 || !strings.Contains(got.LastError, "hk ID") {
		t.Fatalf("op %+v, finds %d, creates %d", got, e.yt.Finds(), e.yt.Calls())
	}
	e.yt.SetIssueFieldsMissing(issueProject, false)
	e.drive()
	if e.want(op.ID, ytwrite.StateApplied, ""); e.yt.Calls() != 1 {
		t.Fatalf("creates %d", e.yt.Calls())
	}
}
