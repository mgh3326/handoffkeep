package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Document copies (hk 4xxx): the task_docs feed consumer prepares one
// comment_append op per part of each changed task-tied report or brief.

// ytdSetup is a throwaway store in document-comment mode on, with the test
// project mapped, one bound task and one unbound task. Documents written
// before the first pass are history, not the feed's (the cursor starts at
// the feed's max_sync_id).
func ytdSetup(t *testing.T, prefix string) (s *Store, bound, unbound int64) {
	t.Helper()
	s = schemaStore(t, prefix)
	s.SetYTDocCommentsMode(YTDocCommentsOn)
	ytbMapped(t, s)
	bound, unbound = ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,'test',now())`, bound, ytcTenant, ytdIssue(bound), ytbProjectID)
	if pass := ytdPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	return s, bound, unbound
}

func ytdIssue(task int64) string { return "2-" + strconv.FormatInt(task, 10) }

func ytdSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// ytdDoc inserts a document (task 0: no task) and returns its id.
func ytdDoc(t *testing.T, s *Store, key, kind, body string, task int64) int64 {
	t.Helper()
	var taskArg any
	if task > 0 {
		taskArg = task
	}
	var id int64
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id)
		VALUES ($1,$2,'','',$3,$4,'worker-a',now(),now(),$5) RETURNING id`, key, kind, body, ytdSHA(body), taskArg).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ytdUpdate rewrites a document's body (its sha follows the body).
func ytdUpdate(t *testing.T, s *Store, id int64, body string) {
	t.Helper()
	ytFeedExec(t, s, `UPDATE documents SET body=$2, sha256=$3, updated_at=now() WHERE id=$1`, id, body, ytdSHA(body))
}

func ytdPass(t *testing.T, s *Store) YTFeedPass {
	t.Helper()
	return ytFeedPassOK(t, s, ytFeedConsumers[ytDocCommentsConsumer])
}

// ytdOps is the document ops of a document, oldest first.
func ytdOps(t *testing.T, s *Store, docID int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE principal=$1 AND kind=$2 AND idem_key LIKE $3 ORDER BY id`,
		ytDocCommentPrincipal, ytwrite.KindCommentAppend, "doc-comment:"+strconv.FormatInt(docID, 10)+":%")
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// ytdBodies is the body of each op's payload (the text before the provenance
// line and marker), in order.
func ytdBodies(t *testing.T, ops []ytwrite.Op) []string {
	t.Helper()
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		var p ytwrite.CommentPayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Body)
	}
	return out
}

func TestYTDocCommentsNewReportMakesOneOp(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_new")
	id := ytdDoc(t, s, "reports/job-a/report.md", "report", "hello\n", bound)
	pass := ytdPass(t, s)
	if pass.Read != 1 {
		t.Fatalf("pass %+v", pass)
	}
	ops := ytdOps(t, s, id)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	op := ops[0]
	if op.Principal != ytDocCommentPrincipal || op.Kind != ytwrite.KindCommentAppend || op.EntityKind != "task" ||
		op.EntityID != strconv.FormatInt(bound, 10) || op.YTTargetID != ytdIssue(bound) || op.Tenant != ytcTenant {
		t.Fatalf("op %+v", op)
	}
	if want := "doc-comment:" + strconv.FormatInt(id, 10) + ":" + ytdSHA("hello\n"); op.Key != want {
		t.Errorf("key %q, want %q", op.Key, want)
	}
	if got := ytdBodies(t, ops); len(got) != 1 || got[0] != "# reports/job-a/report.md\n\nhello\n" {
		t.Errorf("bodies %q", got)
	}
}

// A split report gets one op per part, the import's split: the bodies are
// ytwrite.DocComments of the same text, part for part.
func TestYTDocCommentsSplitReportMakesOneOpPerPart(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_split")
	body := strings.Repeat("a long line of the report text that keeps going\n", 12000)
	id := ytdDoc(t, s, "reports/job-b/report.md", "report", body, bound)
	ytdPass(t, s)
	want, ok, err := ytwrite.DocComments("reports/job-b/report.md", "report", body)
	if err != nil || !ok || len(want) < 2 {
		t.Fatalf("fixture: parts %d ok %v err %v", len(want), ok, err)
	}
	ops := ytdOps(t, s, id)
	if len(ops) != len(want) {
		t.Fatalf("ops %d, want %d", len(ops), len(want))
	}
	got := ytdBodies(t, ops)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d differs", i+1)
		}
	}
	if !strings.HasSuffix(ops[1].Key, ":part:2") {
		t.Errorf("part 2 key %q", ops[1].Key)
	}
}

func TestYTDocCommentsSameShaMakesNoOp(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_same")
	id := ytdDoc(t, s, "reports/job-c/report.md", "report", "same\n", bound)
	ytdPass(t, s)
	ytdUpdate(t, s, id, "same\n")
	ytdPass(t, s)
	if n := len(ytdOps(t, s, id)); n != 1 {
		t.Fatalf("ops %d after an unchanged sha, want 1", n)
	}
}

func TestYTDocCommentsChangedShaMakesNewOps(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_changed")
	id := ytdDoc(t, s, "reports/job-d/report.md", "report", "first\n", bound)
	ytdPass(t, s)
	ytdUpdate(t, s, id, "second\n")
	ytdPass(t, s)
	ops := ytdOps(t, s, id)
	if len(ops) != 2 {
		t.Fatalf("ops %d, want 2 (one per sha)", len(ops))
	}
	if ops[0].Key == ops[1].Key {
		t.Errorf("both ops share key %q", ops[0].Key)
	}
	if got := ytdBodies(t, ops); got[1] != "# reports/job-d/report.md\n\nsecond\n" {
		t.Errorf("second body %q", got[1])
	}
}

func TestYTDocCommentsBriefMakesArticleLine(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_brief")
	id := ytdDoc(t, s, "brief/2026-11-09/job-a", "brief", "the brief body", bound)
	ytdPass(t, s)
	ops := ytdOps(t, s, id)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	if got := ytdBodies(t, ops); got[0] != "Article: [brief] brief/2026-11-09/job-a [brief] (hk document key: brief/2026-11-09/job-a)" {
		t.Errorf("brief body %q", got[0])
	}
}

// Unbound tasks, documents with no task and other kinds of document make no
// op; the cursor still moves past them.
func TestYTDocCommentsSkipsUnboundAndOtherDocuments(t *testing.T) {
	s, _, unbound := ytdSetup(t, "ytd_skip")
	unboundDoc := ytdDoc(t, s, "reports/job-e/report.md", "report", "x\n", unbound)
	noTask := ytdDoc(t, s, "reports/job-f/report.md", "report", "x\n", 0)
	other := ytdDoc(t, s, "decision/2026-11-09/x", "note", "x\n", unbound)
	before := ytFeedCursor(t, s, ytDocCommentsConsumer)
	pass := ytdPass(t, s)
	if pass.Read != 3 || pass.Handled != 3 {
		t.Fatalf("pass %+v", pass)
	}
	for _, id := range []int64{unboundDoc, noTask, other} {
		if n := len(ytdOps(t, s, id)); n != 0 {
			t.Errorf("document %d made %d ops", id, n)
		}
	}
	if after := ytFeedCursor(t, s, ytDocCommentsConsumer); after <= before {
		t.Errorf("cursor did not move past skipped documents: %d -> %d", before, after)
	}
}

// Documents written before the cursor exists are history: the import has
// them, so the feed does not post them.
func TestYTDocCommentsStartsAtCursor(t *testing.T) {
	s := schemaStore(t, "ytd_cursor")
	s.SetYTDocCommentsMode(YTDocCommentsOn)
	ytbMapped(t, s)
	bound := ytcTask(t, s, ytbProject)
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,'test',now())`, bound, ytcTenant, ytdIssue(bound), ytbProjectID)
	history := ytdDoc(t, s, "reports/job-old/report.md", "report", "imported\n", bound)
	if pass := ytdPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	if n := len(ytdOps(t, s, history)); n != 0 {
		t.Fatalf("history document made %d ops", n)
	}
	fresh := ytdDoc(t, s, "reports/job-new/report.md", "report", "fresh\n", bound)
	ytdPass(t, s)
	if n := len(ytdOps(t, s, fresh)); n != 1 {
		t.Fatalf("new document made %d ops, want 1", n)
	}
}

// Mode off prepares nothing and still advances the cursor, so a document
// changed while off is not copied later unless it changes again.
func TestYTDocCommentsModeOff(t *testing.T) {
	s, bound, _ := ytdSetup(t, "ytd_off")
	s.SetYTDocCommentsMode(YTDocCommentsOff)
	off := ytdDoc(t, s, "reports/job-g/report.md", "report", "off\n", bound)
	before := ytFeedCursor(t, s, ytDocCommentsConsumer)
	ytdPass(t, s)
	if n := len(ytdOps(t, s, off)); n != 0 {
		t.Fatalf("mode off made %d ops", n)
	}
	if after := ytFeedCursor(t, s, ytDocCommentsConsumer); after <= before {
		t.Errorf("cursor did not advance while off: %d -> %d", before, after)
	}
	s.SetYTDocCommentsMode(YTDocCommentsOn)
	ytdUpdate(t, s, off, "off\nlater\n")
	ytdPass(t, s)
	if n := len(ytdOps(t, s, off)); n != 1 {
		t.Fatalf("after the mode is on, ops %d, want 1", n)
	}
}
