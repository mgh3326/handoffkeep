package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const ytcTenant = "tenant.youtrack.cloud"

func ytcTask(t *testing.T, s *Store, project string) int64 {
	t.Helper()
	var id int64
	now := time.Now().UTC()
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,project)
		VALUES('ytc-lane','ytc task','implement','backlog','test',$1,$1,$1,$2) RETURNING id`, now, project).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func ytcBinding(task int64) YTCommentCanary {
	id := strconv.FormatInt(task, 10)
	return YTCommentCanary{TaskID: task, Tenant: ytcTenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "operator"}
}

func ytcEnroll(t *testing.T, s *Store, task int64) {
	t.Helper()
	ytcEnrollAs(t, s, ytcBinding(task))
}

// ytcEnrollAs publishes b against the task's mappings as they are now.
func ytcEnrollAs(t *testing.T, s *Store, b YTCommentCanary) {
	t.Helper()
	verified, err := s.YTCanaryCandidate(t.Context(), b.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTCommentCanary(t.Context(), b, verified); err != nil {
		t.Fatalf("enroll %d on %s: %v", b.TaskID, b.Tenant, err)
	}
}

// ytcCounting counts the requests a worker makes.
type ytcCounting struct {
	ytwrite.Remote
	posts, lists atomic.Int64
}

func (c *ytcCounting) CreateComment(ctx context.Context, issueID, text string) (string, error) {
	c.posts.Add(1)
	return c.Remote.CreateComment(ctx, issueID, text)
}

func (c *ytcCounting) ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error) {
	c.lists.Add(1)
	return c.Remote.ListComments(ctx, issueID, pageSize)
}

func ytcCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ytcOpFor is the op linked to a comment, or ok false.
func ytcOpFor(t *testing.T, s *Store, commentID int64) (ytwrite.Op, bool) {
	t.Helper()
	var opID int64
	err := s.pool.QueryRow(t.Context(), `SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1`, commentID).Scan(&opID)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return ytwrite.Op{}, false
		}
		t.Fatal(err)
	}
	op, err := s.YTWriteLedger().ByID(t.Context(), opID)
	if err != nil {
		t.Fatal(err)
	}
	return op, true
}

func TestOneOpPerPersistedComment(t *testing.T) {
	s := schemaStore(t, "ytc_one")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	a, err := s.CreateTaskComment(ctx, task, "worker-a", "첫 코멘트")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTaskComment(ctx, task, "worker-b", "둘째")
	if err != nil {
		t.Fatal(err)
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-key-1"}
	m, replayed, err := s.CreateMCPTaskComment(ctx, task, "MCP 코멘트", w)
	if err != nil || replayed {
		t.Fatalf("mcp comment %v %v", replayed, err)
	}
	again, replayed, err := s.CreateMCPTaskComment(ctx, task, "MCP 코멘트", w)
	if err != nil || !replayed || again.ID != m.ID {
		t.Fatalf("mcp replay %+v %v %v", again, replayed, err)
	}
	seen := map[int64]bool{}
	for _, c := range []TaskComment{a, b, m} {
		op, ok := ytcOpFor(t, s, c.ID)
		if !ok {
			t.Fatalf("comment %d has no op", c.ID)
		}
		if seen[op.ID] {
			t.Fatalf("op %d serves two comments", op.ID)
		}
		seen[op.ID] = true
		if op.Principal != YTCommentPrincipal || op.Key != YTCommentKey(c.ID) || op.Kind != ytwrite.KindCommentAppend ||
			op.EntityKind != "task" || op.EntityID != strconv.FormatInt(task, 10) || op.YTTargetID != "2-"+strconv.FormatInt(task, 10) ||
			op.Actor != c.Author || op.State != ytwrite.StatePrepared || !op.CreatedAt.Equal(c.CreatedAt.UTC()) {
			t.Fatalf("op for comment %d = %+v", c.ID, op)
		}
		text, err := ytwrite.RenderComment(op)
		if err != nil || !strings.Contains(text, "\n\n"+c.Body+"\n\n") {
			t.Fatalf("rendered %q: %v", text, err)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 3 {
		t.Fatalf("%d ops for three persisted comments (one replayed)", n)
	}
	// The first comment op anchors the task's map row.
	first, _ := ytcOpFor(t, s, a.ID)
	row, ok, err := s.YTWriteLedger().MapGet(ctx, "task", strconv.FormatInt(task, 10))
	if err != nil || !ok || row.YTID != "2-"+strconv.FormatInt(task, 10) || row.OpID != first.ID {
		t.Fatalf("task map row %+v %v %v", row, ok, err)
	}
}

func TestOpInsertFailureIsAtomic(t *testing.T) {
	s := schemaStore(t, "ytc_atomic")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION ytc_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected op insert failure'; END; $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TRIGGER ytc_fail BEFORE INSERT ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION ytc_fail()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskComment(ctx, task, "worker", "lost?"); err == nil {
		t.Fatal("comment succeeded although its op could not be stored")
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-atomic"}
	if _, _, err := s.CreateMCPTaskComment(ctx, task, "lost too?", w); err == nil {
		t.Fatal("MCP comment succeeded although its op could not be stored")
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM task_comments WHERE task_id=$1`, task); n != 0 {
		t.Fatalf("%d comments committed without their op", n)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM mcp_write_audit`); n != 0 {
		t.Fatalf("%d audit rows committed without their comment", n)
	}
}

// The op is written on the comment's own connection: a pool of one does not
// deadlock waiting for a second.
func TestCommentEnqueueUsesCallerTransaction(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_pool")
	// Migrate with a normal pool first, so the pool of one only serves the
	// comment.
	migrated, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	migrated.Close()
	var s *Store
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	s, err = Open(t.Context(), url+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := s.CreateTaskComment(ctx, task, "worker", "one connection")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, c.ID); !ok {
		t.Fatal("no op")
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-pool-of-one"}
	m, _, err := s.CreateMCPTaskComment(ctx, task, "one connection over MCP", w)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, m.ID); !ok {
		t.Fatal("no op for the MCP comment")
	}
}

func TestModeOffHasNoWriteSideEffects(t *testing.T) {
	s := schemaStore(t, "ytc_off")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("enrollment wrote %d op/map/link rows", n)
	}
	s.SetYTCommentsMode(YTCommentsOff)
	if _, err := s.CreateTaskComment(ctx, task, "worker", "off"); err != nil {
		t.Fatal(err)
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-off"}
	if _, _, err := s.CreateMCPTaskComment(ctx, task, "off too", w); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("mode off wrote %d op/map/link rows", n)
	}
	// Canary mode, task not enrolled: no op either.
	s.SetYTCommentsMode(YTCommentsCanary)
	other := ytcTask(t, s, "handoffkeep")
	if _, err := s.CreateTaskComment(ctx, other, "worker", "not enrolled"); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 0 {
		t.Fatalf("a task outside the canary got %d ops", n)
	}
}

// What PG accepts still commits unchanged; what cannot be sent becomes a
// locally rejected op, never a guessed target.
func TestCommentLocalRejects(t *testing.T) {
	s := schemaStore(t, "ytc_reject")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	quoted := "the marker looks like <!-- hk-op:12 --> in prose"
	c, err := s.CreateTaskComment(ctx, task, "worker", quoted)
	if err != nil || c.Body != quoted {
		t.Fatalf("PG refused text it accepts today: %+v %v", c, err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonInvalidComment || op.Attempts != 0 {
		t.Fatalf("op for a marker-quoting body = %+v %v", op, ok)
	}

	// A task whose map row names another issue (written after enrollment,
	// which refuses it up front): rejected before any send.
	other := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, other)
	anchor, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{Principal: "test", Kind: ytwrite.KindCommentAppend, Key: "anchor",
		PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "999999", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind,hk_key,yt_id,op_id,created_at) VALUES('task',$1,'2-55555',$2,now())`, strconv.FormatInt(other, 10), anchor.ID); err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateTaskComment(ctx, other, "worker", "mapped elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	op2, ok := ytcOpFor(t, s, c2.ID)
	if !ok || op2.State != ytwrite.StateRejected || op2.Reason != ytwrite.ReasonAmbiguousIssue {
		t.Fatalf("op for a disagreeing map = %+v %v", op2, ok)
	}
}

func TestLegacyMirrorCannotPostProtocolOwnedComment(t *testing.T) {
	s := schemaStore(t, "ytc_legacy")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: "9-1", DisplayRef: "HK-1", Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ytcEnroll(t, s, task)
	s.SetYTCommentsMode(YTCommentsCanary)
	owned, err := s.CreateTaskComment(ctx, task, "worker", "owned by the protocol")
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.DueYouTrackPushTasks(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == task {
			t.Fatal("the legacy push picked a task whose only new comment the protocol owns")
		}
	}
	got, err := s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("legacy push would post %+v (%v)", got, err)
	}
	s.SetYTCommentsMode(YTCommentsOff)
	free, err := s.CreateTaskComment(ctx, task, "worker", "not owned")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(got) != 1 || got[0].ID != free.ID || got[0].ID == owned.ID {
		t.Fatalf("unowned comment not offered to the legacy push: %+v %v", got, err)
	}
}

func TestImportMarksProtocolOwnedComments(t *testing.T) {
	s := schemaStore(t, "ytc_import")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	free, err := s.CreateTaskComment(ctx, task, "worker", "imported normally")
	if err != nil {
		t.Fatal(err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	owned, err := s.CreateTaskComment(ctx, task, "worker", "hk writes this one")
	if err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, owned.ID)
	check := func(name string, rows []ImportComment) {
		t.Helper()
		got := map[int64]int64{}
		for _, r := range rows {
			got[r.ID] = r.HKOpID
		}
		if len(got) != 2 || got[free.ID] != 0 || got[owned.ID] != op.ID {
			t.Fatalf("%s: hk_op_id by comment = %v, want %d -> 0 and %d -> %d", name, got, free.ID, owned.ID, op.ID)
		}
	}
	page, err := s.ImportCommentsPage(ctx, 0, 100, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	check("full page", page)
	updated, err := s.ImportCommentsUpdatedPage(ctx, []int64{task}, time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	check("update page", updated)
}

// ytcProtocol runs the comment worker's protocol (YTCommentConfig) on the
// test tenant over yt.
func ytcProtocol(t *testing.T, s *Store, yt ytwrite.Remote, offset *atomic.Int64) *ytwrite.Protocol {
	t.Helper()
	return ytcProtocolOn(t, s, yt, ytcTenant, offset)
}

// ytcProtocolOn is ytcProtocol for a worker configured with tenant; its clock
// is the wall clock moved by offset.
func ytcProtocolOn(t *testing.T, s *Store, yt ytwrite.Remote, tenant string, offset *atomic.Int64) *ytwrite.Protocol {
	t.Helper()
	p, err := ytwrite.New(s.YTCommentConfig(tenant, ytwrite.Config{Remote: yt,
		Now: func() time.Time { return time.Now().UTC().Add(time.Duration(offset.Load())) }}))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommentProtocolOverPostgres(t *testing.T) {
	s := schemaStore(t, "ytc_proto")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	issue := "2-" + strconv.FormatInt(task, 10)
	ytcEnroll(t, s, task)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)

	c, err := s.CreateTaskComment(ctx, task, "worker", "happy path")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, c.ID)
	cs := yt.Comments(issue)
	if op.State != ytwrite.StateApplied || len(cs) != 1 || op.YTResultID != cs[0].ID {
		t.Fatalf("happy path: op %+v, remote %+v", op, cs)
	}
	actor, at, id, ok := ytwrite.ParseProvenance(strings.SplitN(cs[0].Text, "\n", 2)[0])
	if !ok || actor != "worker" || id != op.ID || !at.Equal(c.CreatedAt.UTC().Truncate(time.Second)) || !strings.HasSuffix(cs[0].Text, ytwrite.OpMarker(op.ID)) {
		t.Fatalf("remote text %q", cs[0].Text)
	}

	yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	lost, err := s.CreateTaskComment(ctx, task, "worker", "lost reply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, lost.ID); op.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("lost reply: %+v", op)
	}
	offset.Store(int64(time.Minute))
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, lost.ID); op.State != ytwrite.StateApplied || yt.Calls() != 2 || yt.Commits() != 2 {
		t.Fatalf("reconciled by marker: %+v, calls %d commits %d", op, yt.Calls(), yt.Commits())
	}

	// Removing the task from the canary freezes sends; the op is not rejected.
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	ytcEnroll(t, s, task)
	held, err := s.CreateTaskComment(ctx, task, "worker", "enrolled when written")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveYTCommentCanary(ctx, task); err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(2 * time.Minute))
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, held.ID); op.State != ytwrite.StatePrepared || yt.Calls() != 2 {
		t.Fatalf("held op: %+v, calls %d", op, yt.Calls())
	}
}

func TestTwoDispatchersOneCreate(t *testing.T) {
	s := schemaStore(t, "ytc_two")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	yt := ytwritetest.NewFake()
	yt.SetLatency(50 * time.Millisecond)
	var offset atomic.Int64
	a, b := ytcProtocol(t, s, yt, &offset), ytcProtocol(t, s, yt, &offset)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateTaskComment(ctx, task, "worker", "race "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, p := range []*ytwrite.Protocol{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				_, _ = p.Drive(ctx, 10)
			}
		}()
	}
	wg.Wait()
	if yt.Calls() != 3 || yt.Commits() != 3 {
		t.Fatalf("two dispatchers made %d calls / %d comments for three ops", yt.Calls(), yt.Commits())
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE state='applied'`); n != 3 {
		t.Fatalf("%d applied", n)
	}
}

func TestListYTWriteOps(t *testing.T) {
	s := schemaStore(t, "ytc_list")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	t1, t2 := ytcTask(t, s, "handoffkeep"), ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, t1)
	ytcEnroll(t, s, t2)
	var ids []int64
	for _, task := range []int64{t1, t1, t2} {
		c, err := s.CreateTaskComment(ctx, task, "worker", "x")
		if err != nil {
			t.Fatal(err)
		}
		op, _ := ytcOpFor(t, s, c.ID)
		ids = append(ids, op.ID)
	}
	rej, err := s.CreateTaskComment(ctx, t2, "worker", "<!-- hk-op:1 -->")
	if err != nil {
		t.Fatal(err)
	}
	rejOp, _ := ytcOpFor(t, s, rej.ID)
	list := func(f YTOpFilter) []int64 {
		t.Helper()
		ops, err := s.ListYTWriteOps(ctx, f)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		var out []int64
		for _, op := range ops {
			out = append(out, op.ID)
		}
		return out
	}
	eq := func(a, b []int64) bool { return fmtIDs(a) == fmtIDs(b) }
	if got := list(YTOpFilter{}); !eq(got, ids) {
		t.Fatalf("default (pending) = %v, want %v", got, ids)
	}
	if got := list(YTOpFilter{State: "all"}); !eq(got, append(append([]int64{}, ids...), rejOp.ID)) {
		t.Fatalf("all = %v", got)
	}
	if got := list(YTOpFilter{State: "rejected"}); !eq(got, []int64{rejOp.ID}) {
		t.Fatalf("rejected = %v", got)
	}
	if got := list(YTOpFilter{TaskID: t1}); !eq(got, ids[:2]) {
		t.Fatalf("task filter = %v", got)
	}
	if got := list(YTOpFilter{AfterID: ids[0], Limit: 1}); !eq(got, ids[1:2]) {
		t.Fatalf("page = %v", got)
	}
	for _, bad := range []YTOpFilter{{State: "done"}, {Limit: YTOpListMax + 1}, {TaskID: -1}, {AfterID: -1}} {
		if _, err := s.ListYTWriteOps(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func fmtIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func TestYTCommentCanarySet(t *testing.T) {
	s := schemaStore(t, "ytc_set")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task, other := ytcTask(t, s, "handoffkeep"), ytcTask(t, s, "handoffkeep")
	add := func(b YTCommentCanary) (YTCommentCanary, error) {
		t.Helper()
		verified, err := s.YTCanaryCandidate(ctx, b.TaskID)
		if errors.Is(err, ErrTaskNotFound) {
			verified = YTCanaryCandidate{TaskID: b.TaskID}
		} else if err != nil {
			t.Fatal(err)
		}
		return s.AddYTCommentCanary(ctx, b, verified)
	}
	got, err := add(ytcBinding(task))
	if err != nil || got.TaskID != task || got.IssueID != ytcBinding(task).IssueID || got.AddedAt.IsZero() {
		t.Fatalf("add = %+v %v", got, err)
	}
	if _, err := add(ytcBinding(task)); err != nil {
		t.Fatalf("same binding again: %v", err)
	}
	moved := ytcBinding(task)
	moved.IssueID = "2-424242"
	if _, err := add(moved); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("another binding for an enrolled task: %v", err)
	}
	stolen := ytcBinding(other)
	stolen.IssueID = ytcBinding(task).IssueID
	if _, err := add(stolen); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("an issue already bound to another task: %v", err)
	}
	if _, err := add(ytcBinding(987654321)); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
	list, err := s.ListYTCommentCanary(ctx)
	if err != nil || len(list) != 1 || list[0].TaskID != task || list[0].Tenant != ytcTenant {
		t.Fatalf("list = %+v %v", list, err)
	}
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || removed {
		t.Fatalf("second remove = %v %v", removed, err)
	}
	if list, err := s.ListYTCommentCanary(ctx); err != nil || len(list) != 0 {
		t.Fatalf("list after removal = %+v %v", list, err)
	}
}

func TestYTCanaryCandidateReadsEveryMapping(t *testing.T) {
	s := schemaStore(t, "ytc_cand")
	ctx := t.Context()
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytc-mapped", ProjectID: "0-9", ShortName: "YTC"}); err != nil {
		t.Fatal(err)
	}
	mapped := ytcTask(t, s, "ytc-mapped")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: mapped, IssueID: "9-1", DisplayRef: "YTC-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.YTCanaryCandidate(ctx, mapped)
	if err != nil || c.TaskID != mapped || c.Project != "ytc-mapped" || c.ShortName != "YTC" || c.ProjectID != "0-9" || c.MirrorIssueID != "9-1" || c.MirrorTenant != ytcTenant || c.MappedIssueID != "" {
		t.Fatalf("candidate = %+v %v", c, err)
	}
	// No OTHER fallback: an unmapped project has no short name.
	loose := ytcTask(t, s, "ytc-unmapped")
	c, err = s.YTCanaryCandidate(ctx, loose)
	if err != nil || c.Project != "ytc-unmapped" || c.ShortName != "" {
		t.Fatalf("unmapped candidate = %+v %v", c, err)
	}
	if _, err := s.YTCanaryCandidate(ctx, 987654321); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

const ytCommentCanaryMigrationID = "20261109-yt-comment-canary"

// A database already current on every other ID gains the new tables when the
// binary that registers this one opens it, and records the ID exactly once.
func TestCanaryMigrationUpgradesAlreadyCurrentDatabase(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_mig")
	var without []migration
	for _, m := range migrationRegistry {
		if m.ID != ytCommentCanaryMigrationID && m.ID != "20261110-yt-comments-on" {
			without = append(without, m)
		}
	}
	old := openRegistry(t, url, without)
	tables := func(s *Store) int {
		return ytcCount(t, s, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('yt_comment_canary','yt_comment_write_links')`)
	}
	if n := tables(old); n != 0 {
		t.Fatalf("tables exist before the migration: %d", n)
	}
	old.Close()
	for i := 0; i < 2; i++ {
		s := openRegistry(t, url, migrationRegistry)
		if n := tables(s); n != 2 {
			t.Fatalf("open %d: %d of 2 tables", i, n)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='yt_write_ops' AND column_name='tenant'`); n != 1 {
			t.Fatalf("open %d: yt_write_ops.tenant columns = %d", i, n)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, ytCommentCanaryMigrationID); n != 1 {
			t.Fatalf("open %d: id recorded %d times", i, n)
		}
		if v := ytcCount(t, s, `SELECT max(version) FROM schema_version`); v != 53 {
			t.Fatalf("integer schema version = %d", v)
		}
		s.Close()
	}
}

func TestModeOnCommentProtocol(t *testing.T) {
	s := schemaStore(t, "ytc_mode_on")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsOn)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)

	// 1. Verified task enqueues op and link, and Drive applies to fake remote.
	taskA := ytcTask(t, s, "handoffkeep")
	issueA := "2-" + strconv.FormatInt(taskA, 10)
	if err := s.AddYTEntityMapTask(ctx, taskA, issueA, "0-2", ytcTenant); err != nil {
		t.Fatalf("verify taskA: %v", err)
	}
	cA, err := s.CreateTaskComment(ctx, taskA, "worker", "comment on verified task A")
	if err != nil {
		t.Fatal(err)
	}
	opA, ok := ytcOpFor(t, s, cA.ID)
	if !ok || opA.State != ytwrite.StatePrepared {
		t.Fatalf("taskA op = %+v, ok=%v, want prepared op", opA, ok)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	opA, _ = ytcOpFor(t, s, cA.ID)
	csA := yt.Comments(issueA)
	if opA.State != ytwrite.StateApplied || len(csA) != 1 || opA.YTResultID != csA[0].ID {
		t.Fatalf("taskA op not applied: op=%+v, remote=%+v", opA, csA)
	}

	// 2. Unverified task gets no op and no link, and legacy-push query still selects the comment.
	taskB := ytcTask(t, s, "handoffkeep")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: taskB, IssueID: "9-2", DisplayRef: "HK-2", Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cB, err := s.CreateTaskComment(ctx, taskB, "worker", "comment on unverified task B")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cB.ID); ok {
		t.Fatal("unverified task B got an op")
	}
	unpushedB, err := s.UnpushedYouTrackComments(ctx, taskB, 0, 50)
	if err != nil || len(unpushedB) != 1 || unpushedB[0].ID != cB.ID {
		t.Fatalf("legacy push did not select unverified comment: %+v %v", unpushedB, err)
	}

	// 3. Ambiguous task (task_origins and mapping disagree) gets no op and no link.
	taskC := ytcTask(t, s, "handoffkeep")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: taskC, IssueID: "9-old", DisplayRef: "OLD-1", Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cC, err := s.CreateTaskComment(ctx, taskC, "worker", "comment on ambiguous task C")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cC.ID); ok {
		t.Fatal("ambiguous task C got an op")
	}

	// 4. Canary mode unchanged: enrolled task enqueues; task outside canary does not enqueue (even if verified in entity map).
	s.SetYTCommentsMode(YTCommentsCanary)
	taskEnrolled := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, taskEnrolled)
	cEnrolled, err := s.CreateTaskComment(ctx, taskEnrolled, "worker", "canary comment")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cEnrolled.ID); !ok {
		t.Fatal("enrolled task got no op in canary mode")
	}
	taskNotEnrolled := ytcTask(t, s, "handoffkeep")
	if err := s.AddYTEntityMapTask(ctx, taskNotEnrolled, "2-notenrolled", "0-2", ytcTenant); err != nil {
		t.Fatal(err)
	}
	cNotEnrolled, err := s.CreateTaskComment(ctx, taskNotEnrolled, "worker", "not enrolled comment")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cNotEnrolled.ID); ok {
		t.Fatal("task outside canary got an op in canary mode")
	}

	// 5. Mode off writes no op rows.
	s.SetYTCommentsMode(YTCommentsOff)
	cOff, err := s.CreateTaskComment(ctx, taskA, "worker", "mode off comment")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cOff.ID); ok {
		t.Fatal("mode off created an op")
	}

	// 6. A task verified later starts getting ops for comments after that point only (no backfill of old comments).
	s.SetYTCommentsMode(YTCommentsOn)
	taskD := ytcTask(t, s, "handoffkeep")
	issueD := "2-" + strconv.FormatInt(taskD, 10)
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: taskD, IssueID: "9-4", DisplayRef: "HK-4", Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cD1, err := s.CreateTaskComment(ctx, taskD, "worker", "comment before verification")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, cD1.ID); ok {
		t.Fatal("unverified task D got an op for cD1")
	}
	// Verify task D now
	if err := s.AddYTEntityMapTask(ctx, taskD, issueD, "0-2", ytcTenant); err != nil {
		t.Fatalf("verify taskD: %v", err)
	}
	// Comment after verification gets op
	cD2, err := s.CreateTaskComment(ctx, taskD, "worker", "comment after verification")
	if err != nil {
		t.Fatal(err)
	}
	opD2, ok := ytcOpFor(t, s, cD2.ID)
	if !ok || opD2.State != ytwrite.StatePrepared {
		t.Fatalf("cD2 op = %+v, ok=%v, want prepared", opD2, ok)
	}
	// cD1 still has no op
	if _, ok := ytcOpFor(t, s, cD1.ID); ok {
		t.Fatal("cD1 was unexpectedly backfilled with an op")
	}
	// Drive only posts cD2
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	csD := yt.Comments(issueD)
	if len(csD) != 1 {
		t.Fatalf("remote issue D has %d comments, want 1", len(csD))
	}
	// Legacy push still selects cD1 but not cD2
	unpushedD, err := s.UnpushedYouTrackComments(ctx, taskD, 0, 50)
	if err != nil || len(unpushedD) != 1 || unpushedD[0].ID != cD1.ID {
		t.Fatalf("legacy push for task D: got %+v, want only cD1 (%d)", unpushedD, cD1.ID)
	}
}

// A task mapped before the tenant column was backfilled (tenant=”) falls back
// to the canary row's tenant, enqueues an op with that tenant, admits and applies it.
// If the map row has tenant=” and no canary row exists to provide the tenant,
// it is treated as unverified: no op and no link are created, leaving the comment
// to legacy push.
func TestYTCommentsOnLegacyMapRowWithoutTenant(t *testing.T) {
	s := schemaStore(t, "ytc_legacy_map")
	ctx := t.Context()
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)

	s.SetYTCommentsMode(YTCommentsOn)

	// 1. Task with legacy map row (tenant='') AND canary row (holding real tenant).
	taskA := ytcTask(t, s, "handoffkeep")
	issueA := "2-" + strconv.FormatInt(taskA, 10)
	verifiedA, err := s.YTCanaryCandidate(ctx, taskA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTCommentCanary(ctx, YTCommentCanary{TaskID: taskA, Tenant: ytcTenant, IssueID: issueA, IssueReadable: "HK-" + strconv.FormatInt(taskA, 10), ProjectID: "0-2", AddedBy: "test"}, verifiedA); err != nil {
		t.Fatal(err)
	}
	// Create an anchor op for yt_entity_map since op_id is NOT NULL
	anchorA, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{
		Principal: YTCommentPrincipal, Kind: ytwrite.KindCommentAppend, Key: "anchor:a",
		PayloadSHA: "sha", Payload: []byte("{}"), EntityKind: "task", EntityID: strconv.FormatInt(taskA, 10),
		YTTargetID: issueA, Actor: "test", Tenant: ytcTenant,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`, anchorA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='c-anchor-a', resolved_at=now() WHERE id=$1`, anchorA.ID); err != nil {
		t.Fatal(err)
	}
	// Insert legacy map row with tenant=''
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id, created_at, tenant)
		VALUES ('task', $1, $2, '0-2', $3, now(), '')`, strconv.FormatInt(taskA, 10), issueA, anchorA.ID); err != nil {
		t.Fatal(err)
	}

	// Post comment in mode on: should resolve tenant from canary and enqueue with ytcTenant
	cA, err := s.CreateTaskComment(ctx, taskA, "worker", "comment on legacy mapped task")
	if err != nil {
		t.Fatal(err)
	}
	opA, ok := ytcOpFor(t, s, cA.ID)
	if !ok || opA.State != ytwrite.StatePrepared || opA.Tenant != ytcTenant {
		t.Fatalf("opA: ok=%v op=%+v, want prepared with tenant %q", ok, opA, ytcTenant)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	opA, _ = ytcOpFor(t, s, cA.ID)
	if opA.State != ytwrite.StateApplied {
		t.Fatalf("opA state = %s, want applied", opA.State)
	}
	csA := yt.Comments(issueA)
	if len(csA) != 1 {
		t.Fatalf("remote issue A comments = %d, want 1", len(csA))
	}

	// 2. Task with legacy map row (tenant='') but NO canary row.
	taskB := ytcTask(t, s, "handoffkeep")
	issueB := "2-" + strconv.FormatInt(taskB, 10)
	anchorB, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{
		Principal: YTCommentPrincipal, Kind: ytwrite.KindCommentAppend, Key: "anchor:b",
		PayloadSHA: "sha", Payload: []byte("{}"), EntityKind: "task", EntityID: strconv.FormatInt(taskB, 10),
		YTTargetID: issueB, Actor: "test", Tenant: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`, anchorB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id='c-anchor-b', resolved_at=now() WHERE id=$1`, anchorB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id, created_at, tenant)
		VALUES ('task', $1, $2, '0-2', $3, now(), '')`, strconv.FormatInt(taskB, 10), issueB, anchorB.ID); err != nil {
		t.Fatal(err)
	}
	// Seed mirror so legacy push is enabled
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: taskB, IssueID: issueB, DisplayRef: "HK-" + strconv.FormatInt(taskB, 10), Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	cB, err := s.CreateTaskComment(ctx, taskB, "worker", "comment on tenantless map row")
	if err != nil {
		t.Fatal(err)
	}
	// No op and no link should be created because tenant cannot be resolved
	if _, ok := ytcOpFor(t, s, cB.ID); ok {
		t.Fatal("tenantless map row unexpectedly enqueued an op")
	}
	unpushedB, err := s.UnpushedYouTrackComments(ctx, taskB, 0, 50)
	if err != nil || len(unpushedB) != 1 || unpushedB[0].ID != cB.ID {
		t.Fatalf("legacy push did not select comment cB: got %+v %v", unpushedB, err)
	}
}
