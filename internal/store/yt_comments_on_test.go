package store

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Comment mode on (hk 3810): a task's yt_task_bindings row is the only
// authority for where its comments are copied, and every other mapping hk
// keeps must agree with it.

const (
	ytbProject   = "ytb-project"
	ytbProjectID = "0-2"
)

// ytbMapped maps the test project to the YouTrack project the bindings name.
func ytbMapped(t *testing.T, s *Store) {
	t.Helper()
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: ytbProject, ProjectID: ytbProjectID, ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
}

func ytbBinding(task int64) YTTaskBinding {
	id := strconv.FormatInt(task, 10)
	return YTTaskBinding{TaskID: task, Tenant: ytcTenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: ytbProjectID, BoundBy: "operator"}
}

// ytbAdd publishes b against the task's mappings as they are now.
func ytbAdd(t *testing.T, s *Store, b YTTaskBinding) (YTTaskBinding, error) {
	t.Helper()
	verified, err := s.YTCanaryCandidate(t.Context(), b.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return s.AddYTTaskBinding(t.Context(), b, verified)
}

func ytbBind(t *testing.T, s *Store, b YTTaskBinding) {
	t.Helper()
	if _, err := ytbAdd(t, s, b); err != nil {
		t.Fatalf("bind %d to %s on %s: %v", b.TaskID, b.IssueID, b.Tenant, err)
	}
}

func ytbComment(t *testing.T, s *Store, task int64, body string) CreatedTaskComment {
	t.Helper()
	c, err := s.CreateTaskCommentProjected(t.Context(), task, "worker", body)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ytbLegacyCarries asserts that comment c has no op and no ownership link,
// so the legacy push still selects it.
func ytbLegacyCarries(t *testing.T, s *Store, c TaskComment) {
	t.Helper()
	if op, ok := ytcOpFor(t, s, c.ID); ok {
		t.Fatalf("comment %d got op %+v", c.ID, op)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_comment_write_links WHERE comment_id=$1`, c.ID); n != 0 {
		t.Fatalf("comment %d has %d ownership links", c.ID, n)
	}
	got, err := s.UnpushedYouTrackComments(t.Context(), c.TaskID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(x TaskComment) bool { return x.ID == c.ID }) {
		t.Fatalf("the legacy push does not select comment %d: %+v", c.ID, got)
	}
}

// ytbAnchor writes a yt_entity_map task row; its op is a throwaway on an
// unrelated lane, moved by nothing.
func ytbAnchor(t *testing.T, s *Store, task int64, issue string) {
	t.Helper()
	key := strconv.FormatInt(task, 10)
	anchor, _, err := s.YTWriteLedger().Prepare(t.Context(), time.Now().UTC(), ytwrite.NewOp{Principal: "test", Kind: ytwrite.KindCommentAppend,
		Key: "anchor-" + key + "-" + issue, PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "999999", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_entity_map(entity_kind,hk_key,yt_id,op_id,created_at) VALUES('task',$1,$2,$3,now())`, key, issue, anchor.ID); err != nil {
		t.Fatal(err)
	}
}

// ytbCanaryRow writes a yt_comment_canary row directly: the state an
// enrollment by a previous release could leave, which this release's
// AddYTCommentCanary would refuse.
func ytbCanaryRow(t *testing.T, s *Store, task int64, tenant, issue string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_comment_canary(task_id,tenant,issue_id,issue_readable,project_id,added_by,added_at)
		VALUES($1,$2,$3,'',$4,'old-release',now())`, task, tenant, issue, ytbProjectID); err != nil {
		t.Fatal(err)
	}
}

// ytbLog captures the standard logger for the rest of the test.
func ytbLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// T1: a bound, consistent task gets one op, its link and its map anchor in
// the comment's transaction, and the worker delivers exactly one comment to
// the binding's issue.
func TestYTCommentsOnBoundTaskEnqueuesAndDelivers(t *testing.T) {
	s := schemaStore(t, "ytb_t1")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	b := ytbBinding(task)
	ytbBind(t, s, b)
	s.SetYTCommentsMode(YTCommentsOn)
	c := ytbComment(t, s, task, "모드 on 코멘트")
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared || op.Tenant != b.Tenant || op.YTTargetID != b.IssueID ||
		op.EntityKind != "task" || op.EntityID != strconv.FormatInt(task, 10) || op.Key != YTCommentKey(c.ID) {
		t.Fatalf("op for a bound task = %+v %v", op, ok)
	}
	if c.YouTrackProjection == nil || c.YouTrackProjection.Status != string(ytwrite.StatusPending) || c.YouTrackProjection.OpID != op.ID {
		t.Fatalf("projection %+v", c.YouTrackProjection)
	}
	row, ok, err := s.YTWriteLedger().MapGet(ctx, "task", strconv.FormatInt(task, 10))
	if err != nil || !ok || row.YTID != b.IssueID || row.OpID != op.ID {
		t.Fatalf("task map anchor %+v %v %v", row, ok, err)
	}
	if got, err := s.UnpushedYouTrackComments(ctx, task, 0, 50); err != nil || len(got) != 0 {
		t.Fatalf("the legacy push would also post %+v (%v)", got, err)
	}
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	for i := 0; i < 2; i++ {
		if _, err := p.Drive(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	op, _ = ytcOpFor(t, s, c.ID)
	cs := yt.Comments(b.IssueID)
	if op.State != ytwrite.StateApplied || len(cs) != 1 || cs[0].ID != op.YTResultID || yt.Calls() != 1 {
		t.Fatalf("delivery: op %+v, remote %+v, %d calls", op, cs, yt.Calls())
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 1 {
		t.Fatalf("%d ops for one comment", n)
	}
}

// T2: an unbound task in mode on gets no op and no link, through either
// comment entry point; the legacy push keeps carrying its comments.
func TestYTCommentsOnUnboundTaskWritesNothing(t *testing.T) {
	s := schemaStore(t, "ytb_t2")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	c := ytbComment(t, s, task, "no binding")
	if c.YouTrackProjection == nil || c.YouTrackProjection.Status != YTProjectionNone {
		t.Fatalf("projection %+v", c.YouTrackProjection)
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "ytb-unbound"}
	m, _, err := s.CreateMCPTaskCommentProjected(ctx, task, "no binding over MCP", w)
	if err != nil || m.YouTrackProjection == nil || m.YouTrackProjection.Status != YTProjectionNone {
		t.Fatalf("mcp comment %+v %v", m, err)
	}
	for _, x := range []TaskComment{c.TaskComment, m.TaskComment} {
		ytbLegacyCarries(t, s, x)
	}
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("an unbound task wrote %d op/map/link rows", n)
	}
}

// T3: any local source that disagrees with the binding skips the op (no op,
// no link, one log event naming the source); the legacy push keeps the
// comment.
func TestYTCommentsOnAmbiguousBindingSkips(t *testing.T) {
	s := schemaStore(t, "ytb_t3")
	ctx := t.Context()
	ytbMapped(t, s)
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytb-elsewhere", ProjectID: "0-99", ShortName: "ELS"}); err != nil {
		t.Fatal(err)
	}
	logs := ytbLog(t)
	s.SetYTCommentsMode(YTCommentsOn)
	cases := []struct {
		name, source string
		disagree     func(task int64, b YTTaskBinding)
	}{
		{"mirror names another issue", "task_origins", func(task int64, b YTTaskBinding) {
			if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: "9-" + strconv.FormatInt(task, 10), DisplayRef: "OLD-1", Tenant: b.Tenant, Role: "mirror"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"mirror is on another tenant", "task_origins", func(task int64, b YTTaskBinding) {
			if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: b.IssueID, DisplayRef: "HK-1", Tenant: "other.youtrack.cloud", Role: "mirror"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"map row names another issue", "yt_entity_map_task", func(task int64, _ YTTaskBinding) {
			ytbAnchor(t, s, task, "2-55"+strconv.FormatInt(task, 10))
		}},
		{"map row gives the issue to another task", "yt_entity_map_issue", func(_ int64, b YTTaskBinding) {
			ytbAnchor(t, s, ytcTask(t, s, ytbProject), b.IssueID)
		}},
		{"canary row names another issue", "yt_comment_canary_task", func(task int64, b YTTaskBinding) {
			ytbCanaryRow(t, s, task, b.Tenant, "2-77"+strconv.FormatInt(task, 10))
		}},
		{"canary row is on another tenant", "yt_comment_canary_task", func(task int64, b YTTaskBinding) {
			ytbCanaryRow(t, s, task, "other.youtrack.cloud", b.IssueID)
		}},
		{"canary row gives the issue to another task", "yt_comment_canary_issue", func(_ int64, b YTTaskBinding) {
			ytbCanaryRow(t, s, ytcTask(t, s, ytbProject), b.Tenant, b.IssueID)
		}},
		{"task moved to a project mapped elsewhere", "youtrack_projects", func(task int64, _ YTTaskBinding) {
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project='ytb-elsewhere' WHERE id=$1`, task); err != nil {
				t.Fatal(err)
			}
		}},
		{"task moved to an unmapped project", "youtrack_projects", func(task int64, _ YTTaskBinding) {
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project='ytb-unmapped' WHERE id=$1`, task); err != nil {
				t.Fatal(err)
			}
		}},
		{"task lost its project", "youtrack_projects", func(task int64, _ YTTaskBinding) {
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=NULL WHERE id=$1`, task); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := ytcTask(t, s, ytbProject)
			b := ytbBinding(task)
			ytbBind(t, s, b)
			tc.disagree(task, b)
			c := ytbComment(t, s, task, "ambiguous: "+tc.name)
			if c.YouTrackProjection == nil || c.YouTrackProjection.Status != YTProjectionNone {
				t.Fatalf("projection %+v", c.YouTrackProjection)
			}
			ytbLegacyCarries(t, s, c.TaskComment)
			if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE entity_kind='task' AND entity_id=$1`, strconv.FormatInt(task, 10)); n != 0 {
				t.Fatalf("%d ops on the ambiguous task's lane", n)
			}
			want := "ytwrite.comment_skipped_ambiguous task_id=" + strconv.FormatInt(task, 10) + " comment_id=" + strconv.FormatInt(c.ID, 10) + " source=" + tc.source
			if got := logs(); !strings.Contains(got, want) {
				t.Fatalf("log has no %q:\n%s", want, got)
			}
			if strings.Contains(logs(), c.Body) {
				t.Fatal("the skip log carries the comment body")
			}
		})
	}
}

// T4: canary mode is unchanged: it reads only yt_comment_canary, so a bound
// task that is not enrolled gets no op.
func TestYTCommentsCanaryModeIgnoresBindings(t *testing.T) {
	s := schemaStore(t, "ytb_t4")
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsCanary)
	enrolled, bound := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytcEnroll(t, s, enrolled)
	ytbBind(t, s, ytbBinding(bound))
	c := ytbComment(t, s, enrolled, "enrolled")
	if op, ok := ytcOpFor(t, s, c.ID); !ok || op.YTTargetID != ytcBinding(enrolled).IssueID {
		t.Fatalf("an enrolled task in canary mode: op %+v %v", op, ok)
	}
	ytbLegacyCarries(t, s, ytbComment(t, s, bound, "bound, not enrolled").TaskComment)
}

// T5: mode off writes no op row, bound or not.
func TestYTCommentsOffIgnoresBindings(t *testing.T) {
	s := schemaStore(t, "ytb_t5")
	ytbMapped(t, s)
	bound, unbound := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(bound))
	for _, set := range []bool{false, true} {
		if set {
			s.SetYTCommentsMode(YTCommentsOff)
		}
		for _, task := range []int64{bound, unbound} {
			ytbLegacyCarries(t, s, ytbComment(t, s, task, "off").TaskComment)
		}
	}
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("mode off wrote %d op/map/link rows", n)
	}
}

// T6: a binding added later covers only the comments written after it; the
// earlier ones stay with the legacy push and are never backfilled.
func TestYTCommentsOnLateBinding(t *testing.T) {
	s := schemaStore(t, "ytb_t6")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	early := []TaskComment{ytbComment(t, s, task, "before 1").TaskComment, ytbComment(t, s, task, "before 2").TaskComment}
	ytbBind(t, s, ytbBinding(task))
	late := ytbComment(t, s, task, "after")
	if op, ok := ytcOpFor(t, s, late.ID); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("comment after the binding: %+v %v", op, ok)
	}
	for _, c := range early {
		ytbLegacyCarries(t, s, c)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 1 {
		t.Fatalf("%d ops after a late binding, want 1", n)
	}
	got, err := s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(got) != 2 || got[0].ID != early[0].ID || got[1].ID != early[1].ID {
		t.Fatalf("legacy selection %+v %v", got, err)
	}
}

// T7: in mode on the worker admits a prepared op only while its task has a
// binding with the op's issue on the op's tenant.
func TestYTCommentsOnAdmission(t *testing.T) {
	s := schemaStore(t, "ytb_t7")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytcEnroll(t, s, task)
	s.SetYTCommentsMode(YTCommentsCanary)
	c := ytbComment(t, s, task, "prepared in canary")
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("canary op %+v %v", op, ok)
	}
	s.SetYTCommentsMode(YTCommentsOn)
	ledger := s.YTTenantLedger(ytcTenant)
	now := time.Now().UTC().Add(time.Minute)
	held := func(why string) {
		t.Helper()
		due, err := ledger.Due(ctx, now, 50)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(due, func(o ytwrite.Op) bool { return o.ID == op.ID }) {
			t.Fatalf("%s: Due lists the op", why)
		}
		got, out, err := ledger.Claim(ctx, now, op.ID)
		if err != nil || out != ytwrite.ClaimHeld || got.State != ytwrite.StatePrepared {
			t.Fatalf("%s: claim %s %+v %v, want held", why, out, got, err)
		}
	}
	held("no binding")

	if _, err := s.RemoveYTCommentCanary(ctx, task); err != nil {
		t.Fatal(err)
	}
	elsewhere := ytbBinding(task)
	elsewhere.Tenant = "other.youtrack.cloud"
	ytbBind(t, s, elsewhere)
	held("binding on another tenant")

	if _, err := s.pool.Exec(ctx, `DELETE FROM yt_task_bindings WHERE task_id=$1`, task); err != nil {
		t.Fatal(err)
	}
	ytbBind(t, s, ytbBinding(task))
	due, err := ledger.Due(ctx, now, 50)
	if err != nil || !slices.ContainsFunc(due, func(o ytwrite.Op) bool { return o.ID == op.ID }) {
		t.Fatalf("matching binding: Due %+v %v", due, err)
	}
	if got, out, err := ledger.Claim(ctx, now, op.ID); err != nil || out != ytwrite.ClaimClaimed || got.State != ytwrite.StateDispatched {
		t.Fatalf("matching binding: claim %s %+v %v", out, got, err)
	}
}

// T8: a binding is published only against the mappings it was verified
// with, and never beside a canary row, map row or binding that names the
// task or its issue otherwise.
func TestAddYTTaskBindingRefuses(t *testing.T) {
	s := schemaStore(t, "ytb_t8")
	ctx := t.Context()
	ytbMapped(t, s)
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytb-other", ProjectID: "0-9", ShortName: "OTH"}); err != nil {
		t.Fatal(err)
	}
	refused := func(t *testing.T, b YTTaskBinding, verified YTCanaryCandidate) {
		t.Helper()
		if _, err := s.AddYTTaskBinding(ctx, b, verified); !errors.Is(err, ErrYTBindingConflict) {
			t.Fatalf("err = %v, want ErrYTBindingConflict", err)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings WHERE task_id=$1 AND issue_id=$2 AND tenant=$3`, b.TaskID, b.IssueID, b.Tenant); n != 0 {
			t.Fatal("the refused binding was published")
		}
	}
	// Mappings that changed between verification and publication. The
	// first two are changes only the snapshot comparison sees: every
	// source still agrees with the binding afterwards.
	changed := []struct {
		name   string
		change func(task int64)
	}{
		{"project short name renamed", func(int64) {
			if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytb-rename", ProjectID: ytbProjectID, ShortName: "HKR"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"agreeing mirror link added", func(task int64) {
			if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: ytbBinding(task).IssueID, DisplayRef: "HK-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"task moved to another project", func(task int64) {
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project='ytb-other' WHERE id=$1`, task); err != nil {
				t.Fatal(err)
			}
		}},
		{"mirror link names another issue", func(task int64) {
			if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: "9-1", DisplayRef: "OLD-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytb-rename", ProjectID: ytbProjectID, ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range changed {
		t.Run(tc.name, func(t *testing.T) {
			project := ytbProject
			if tc.name == "project short name renamed" {
				project = "ytb-rename"
			}
			task := ytcTask(t, s, project)
			verified, err := s.YTCanaryCandidate(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(task)
			refused(t, ytbBinding(task), verified)
		})
	}
	current := func(task int64) YTCanaryCandidate {
		v, err := s.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Run("another task's binding holds the issue", func(t *testing.T) {
		task, other := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
		ytbBind(t, s, ytbBinding(other))
		b := ytbBinding(task)
		b.IssueID = ytbBinding(other).IssueID
		refused(t, b, current(task))
	})
	t.Run("another task's canary row names the issue", func(t *testing.T) {
		task, other := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
		ytcEnroll(t, s, other)
		b := ytbBinding(task)
		b.IssueID = ytcBinding(other).IssueID
		refused(t, b, current(task))
	})
	t.Run("another task's map row names the issue", func(t *testing.T) {
		task, other := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
		ytbAnchor(t, s, other, ytbBinding(task).IssueID)
		refused(t, ytbBinding(task), current(task))
	})
	t.Run("own canary row names another issue", func(t *testing.T) {
		task := ytcTask(t, s, ytbProject)
		enrolled := ytcBinding(task)
		enrolled.IssueID = "2-404" + strconv.FormatInt(task, 10)
		ytcEnrollAs(t, s, enrolled)
		refused(t, ytbBinding(task), current(task))
	})
	t.Run("own map row names another issue", func(t *testing.T) {
		task := ytcTask(t, s, ytbProject)
		ytbAnchor(t, s, task, "2-505"+strconv.FormatInt(task, 10))
		refused(t, ytbBinding(task), current(task))
	})
	t.Run("task already bound to another issue", func(t *testing.T) {
		task := ytcTask(t, s, ytbProject)
		ytbBind(t, s, ytbBinding(task))
		b := ytbBinding(task)
		b.IssueID = "2-606" + strconv.FormatInt(task, 10)
		refused(t, b, current(task))
	})
	t.Run("same binding again is a no-op", func(t *testing.T) {
		task := ytcTask(t, s, ytbProject)
		first, err := ytbAdd(t, s, ytbBinding(task))
		if err != nil || first.BoundAt.IsZero() || first.IssueID != ytbBinding(task).IssueID {
			t.Fatalf("first add %+v %v", first, err)
		}
		again := ytbBinding(task)
		again.BoundBy = "someone else"
		second, err := ytbAdd(t, s, again)
		if err != nil || second != first {
			t.Fatalf("second add %+v %v, want %+v", second, err, first)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings WHERE task_id=$1`, task); n != 1 {
			t.Fatalf("%d rows", n)
		}
	})
	t.Run("missing task", func(t *testing.T) {
		if _, err := s.AddYTTaskBinding(ctx, ytbBinding(987654321), YTCanaryCandidate{TaskID: 987654321}); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	})
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops WHERE principal<>'test')+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("binding writes created %d op/link rows", n)
	}
}

// T9: the canary enrollment refuses an issue a binding gives to the task
// differently, or to another task.
func TestAddYTCommentCanaryRefusesDisagreeingBinding(t *testing.T) {
	s := schemaStore(t, "ytb_t9")
	ytbMapped(t, s)
	add := func(c YTCommentCanary) error {
		t.Helper()
		verified, err := s.YTCanaryCandidate(t.Context(), c.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.AddYTCommentCanary(t.Context(), c, verified)
		return err
	}
	task, other := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	moved := ytcBinding(task)
	moved.IssueID = "2-707" + strconv.FormatInt(task, 10)
	if err := add(moved); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("canary on another issue than the task's binding: %v", err)
	}
	elsewhere := ytcBinding(task)
	elsewhere.Tenant = "other.youtrack.cloud"
	if err := add(elsewhere); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("canary on another tenant than the task's binding: %v", err)
	}
	stolen := ytcBinding(other)
	stolen.IssueID = ytbBinding(task).IssueID
	if err := add(stolen); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("canary on an issue bound to another task: %v", err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_comment_canary`); n != 0 {
		t.Fatalf("%d refused enrollments were published", n)
	}
	if err := add(ytcBinding(task)); err != nil {
		t.Fatalf("canary agreeing with the binding: %v", err)
	}
}

const ytTaskBindingsMigrationID = "20261110-yt-task-bindings"

// A database already current on every other ID gains yt_task_bindings when
// the binary that registers this one opens it, and records the ID once. The
// entries that alter the table (After it, hk 3898) are left out with it.
func TestYTTaskBindingsMigrationUpgradesAlreadyCurrentDatabase(t *testing.T) {
	_, url := testdb.Schema(t, "ytb_mig")
	var without []migration
	dependents := 0
	for _, m := range migrationRegistry {
		switch {
		case m.ID == ytTaskBindingsMigrationID:
		case slices.Contains(m.After, ytTaskBindingsMigrationID):
			dependents++
		default:
			without = append(without, m)
		}
	}
	if len(without) != len(migrationRegistry)-1-dependents {
		t.Fatalf("migration %s is not registered", ytTaskBindingsMigrationID)
	}
	old := openRegistry(t, url, without)
	table := func(s *Store) int {
		return ytcCount(t, s, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='yt_task_bindings'`)
	}
	if n := table(old); n != 0 {
		t.Fatalf("yt_task_bindings exists before its migration")
	}
	old.Close()
	for i := 0; i < 2; i++ {
		s := openRegistry(t, url, migrationRegistry)
		if n := table(s); n != 1 {
			t.Fatalf("open %d: yt_task_bindings missing", i)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, ytTaskBindingsMigrationID); n != 1 {
			t.Fatalf("open %d: id recorded %d times", i, n)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings`); n != 0 {
			t.Fatalf("open %d: the migration wrote %d rows", i, n)
		}
		s.Close()
	}
}

// An absent origin cannot change under a binding's publication. With no
// task_origins row there is nothing to lock but the task row, and a first
// mirror link (UpsertYouTrackMirror) reaches it only through its foreign
// key's KEY SHARE. AddYTTaskBinding holds FOR UPDATE on the task row from
// before its re-read until its commit, so a first mirror link that starts
// after every check has passed waits for the binding's commit, and the next
// comment in mode on finds the two disagreeing and is skipped. Under FOR
// SHARE the link commits inside the window and the binding is published
// against mappings that no longer hold.
func TestAddYTTaskBindingHoldsFirstMirrorLinkUntilCommit(t *testing.T) {
	dbURL, app := lockOrderSchema(t, "ytb_origin")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	binder := openLockActorStore(t, dbURL, app("binder"))
	mirror := openLockActorStore(t, dbURL, app("mirror"))
	ytbMapped(t, binder)
	task := ytcTask(t, binder, ytbProject)
	verified, err := binder.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if verified.MirrorIssueID != "" {
		t.Fatalf("fixture: task %d already has a mirror link: %+v", task, verified)
	}
	conflicting := "9-" + strconv.FormatInt(task, 10)
	var link *lockActor
	binder.ytBindingChecked = func() {
		link = startLockActor(app("mirror"), func() error {
			_, err := mirror.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: conflicting, DisplayRef: "OLD-1", Tenant: ytcTenant, Role: "mirror"})
			return err
		})
		if !waitBlockedBy(t, ctx, mirror.pool, link, app("binder")) {
			t.Errorf("the first mirror link committed while the binding's publication held the task row (err %v)", link.err)
		}
	}
	_, err = binder.AddYTTaskBinding(ctx, ytbBinding(task), verified)
	binder.ytBindingChecked = nil
	if err != nil {
		t.Fatalf("publication: %v", err)
	}
	if link == nil {
		t.Fatal("the publication never reached the point after its checks")
	}
	finishLockActors(t, ctx, link)
	if n := ytcCount(t, binder, `SELECT count(*) FROM task_origins WHERE task_id=$1 AND provider='youtrack' AND external_id=$2`, task, conflicting); n != 1 {
		t.Fatalf("the mirror link to %s did not commit after the binding (%d rows)", conflicting, n)
	}

	logs := ytbLog(t)
	binder.SetYTCommentsMode(YTCommentsOn)
	c := ytbComment(t, binder, task, "after the late mirror link")
	ytbLegacyCarries(t, binder, c.TaskComment)
	want := "ytwrite.comment_skipped_ambiguous task_id=" + strconv.FormatInt(task, 10) + " comment_id=" + strconv.FormatInt(c.ID, 10) + " source=task_origins"
	if !strings.Contains(logs(), want) {
		t.Errorf("log %q does not contain %q", logs(), want)
	}
}

// OpenReadOnly runs no migration and its pool refuses writes.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytb_ro")
	s, err := OpenReadOnly(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var tables int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_tables WHERE schemaname=current_schema()`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("OpenReadOnly migrated: %d tables in the new schema", tables)
	}
	applied, err := s.MigrationApplied(t.Context(), YTTaskBindingsMigration)
	if err != nil || applied {
		t.Fatalf("MigrationApplied on an empty schema = %v, %v", applied, err)
	}
	_, err = s.pool.Exec(t.Context(), `CREATE TABLE ro_probe (id int)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("a write through OpenReadOnly: err %v, want SQLSTATE 25006 (read_only_sql_transaction)", err)
	}
}
