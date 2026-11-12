package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The per-kind authority row (hk 4122, docs/youtrack-write-protocol.md
// §Authority).

func ytaSides(t *testing.T, rows []YTAuthority) map[YTAuthorityKind]YTAuthoritySide {
	t.Helper()
	out := map[YTAuthorityKind]YTAuthoritySide{}
	for _, a := range rows {
		out[a.Kind] = a.Authority
	}
	if len(out) != len(YTAuthorityKinds) || len(rows) != len(YTAuthorityKinds) {
		t.Fatalf("rows %+v: want one per kind", rows)
	}
	return out
}

func ytaAll(t *testing.T, s *Store) map[YTAuthorityKind]YTAuthoritySide {
	t.Helper()
	rows, err := s.YTAuthorities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return ytaSides(t, rows)
}

func TestYTAuthorityDefaultRows(t *testing.T) {
	s := schemaStore(t, "yta_default")
	rows, err := s.YTAuthorities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range rows {
		if a.Kind != YTAuthorityKinds[i] || a.Authority != YTAuthorityHK || a.SwitchedAt != nil || a.SwitchedBy != "" || a.Note != "" {
			t.Fatalf("row %d = %+v", i, a)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_authority WHERE authority='hk'`); n != 4 {
		t.Fatalf("%d seeded hk rows, want 4", n)
	}
	// A missing row reads as hk.
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM yt_authority WHERE kind='comments'`); err != nil {
		t.Fatal(err)
	}
	if side, err := s.YTAuthorityOf(t.Context(), YTAuthorityComments); err != nil || side != YTAuthorityHK {
		t.Fatalf("missing comments row = %q %v", side, err)
	}
	if got := ytaAll(t, s); got[YTAuthorityComments] != YTAuthorityHK {
		t.Fatalf("missing row lists as %v", got)
	}
	// The schema refuses an unknown kind or side.
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_authority(kind) VALUES ('files')`); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_authority SET authority='both' WHERE kind='tasks'`); err == nil {
		t.Fatal("unknown authority accepted")
	}
}

func TestYTAuthoritySetRefusals(t *testing.T) {
	s := schemaStore(t, "yta_refuse")
	ctx := t.Context()
	set := func(kinds []YTAuthorityKind, side YTAuthoritySide) ([]YTAuthority, []YTAuthority, error) {
		return s.SetYTAuthority(ctx, kinds, side, "operator", "drill")
	}
	refused := func(err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrYTAuthorityRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %v naming %q", err, ErrYTAuthorityRefused, want)
		}
	}
	unchanged := func() {
		t.Helper()
		for k, side := range ytaAll(t, s) {
			if side != YTAuthorityHK {
				t.Fatalf("%s is %s after a refused set", k, side)
			}
		}
	}

	// Invalid requests change nothing.
	for _, tc := range []struct {
		kinds    []YTAuthorityKind
		side     YTAuthoritySide
		by, note string
	}{
		{nil, YTAuthorityYouTrack, "operator", "n"},
		{[]YTAuthorityKind{"files"}, YTAuthorityYouTrack, "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments, YTAuthorityComments}, YTAuthorityHK, "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, "both", "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "operator", " "},
	} {
		if _, _, err := s.SetYTAuthority(ctx, tc.kinds, tc.side, tc.by, tc.note); err == nil || errors.Is(err, ErrYTAuthorityRefused) {
			t.Fatalf("set %+v = %v, want a validation error", tc, err)
		}
	}

	// Every write mode off: every kind is refused.
	for _, k := range YTAuthorityKinds {
		_, _, err := set([]YTAuthorityKind{k}, YTAuthorityYouTrack)
		refused(err, string(k)+": ")
	}
	unchanged()

	// Comments mode canary is not on.
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	c, err := s.CreateTaskComment(ctx, task, "worker", "pending copy")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("op %+v %v", op, ok)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "HANDOFFKEEP_YT_COMMENTS_MODE")

	// Mode on, but an op is prepared, then dispatched, then remote_unknown.
	s.SetYTCommentsMode(YTCommentsOn)
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='remote_unknown' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	unchanged()

	// An op of another kind's principal does not hold comments: once the
	// comment op is resolved, comments switch while tasks (modes off) are
	// still refused, and `all` changes nothing.
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='rejected', reason='test', resolved_at=now() WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set(YTAuthorityKinds, YTAuthorityYouTrack)
	refused(err, "tasks: ")
	unchanged()
	before, after, err := set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	if err != nil {
		t.Fatal(err)
	}
	if b, a := ytaSides(t, before), ytaSides(t, after); b[YTAuthorityComments] != YTAuthorityHK || a[YTAuthorityComments] != YTAuthorityYouTrack ||
		a[YTAuthorityTasks] != YTAuthorityHK || a[YTAuthorityDocuments] != YTAuthorityHK || a[YTAuthorityDecisions] != YTAuthorityHK {
		t.Fatalf("before %+v after %+v", before, after)
	}
	for _, a := range after {
		if a.Kind == YTAuthorityComments && (a.SwitchedAt == nil || a.SwitchedBy != "operator" || a.Note != "drill") {
			t.Fatalf("switched row %+v", a)
		}
	}

	// Tasks need both the issue and the field modes; decisions are refused in
	// this release whatever the modes.
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "HANDOFFKEEP_YT_FIELDS_MODE")
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	if _, _, err := set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityDecisions}, YTAuthorityYouTrack)
	refused(err, "decisions: ")

	// Rollback to hk needs nothing, even with every mode off again.
	s.SetYTCommentsMode(YTCommentsOff)
	s.SetYTIssuesMode(YTIssuesOff, "")
	s.SetYTFieldsMode(YTFieldsOff, "")
	if _, _, err := set(YTAuthorityKinds, YTAuthorityHK); err != nil {
		t.Fatal(err)
	}
	unchanged()
}

// An unsendable comment: under hk it commits with a born-rejected op, as
// today; under youtrack it is refused and nothing is written; back under hk
// it commits again.
func TestYTAuthorityUnsendableComment(t *testing.T) {
	s := schemaStore(t, "yta_comment")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	const quoted = "the marker looks like <!-- hk-op:12 --> in prose"
	assertBornRejected := func() {
		t.Helper()
		c, err := s.CreateTaskComment(ctx, task, "worker", quoted)
		if err != nil {
			t.Fatalf("hk: %v", err)
		}
		op, ok := ytcOpFor(t, s, c.ID)
		if !ok || op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonInvalidComment {
			t.Fatalf("hk: op %+v %v", op, ok)
		}
	}
	assertBornRejected()

	s.SetYTCommentsMode(YTCommentsOn)
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "operator", "drill"); err != nil {
		t.Fatal(err)
	}
	// Refused in every mode, on an enrolled and on an unenrolled task, through
	// REST and MCP writers alike.
	other := ytcTask(t, s, "handoffkeep")
	comments := ytcCount(t, s, `SELECT count(*) FROM task_comments`)
	ops := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`)
	for _, mode := range []YTCommentsMode{YTCommentsCanary, YTCommentsOn, YTCommentsOff} {
		s.SetYTCommentsMode(mode)
		for _, id := range []int64{task, other} {
			if _, err := s.CreateTaskComment(ctx, id, "worker", quoted); !errors.Is(err, ErrYTCommentUnsendable) || !strings.Contains(err.Error(), "marker") {
				t.Fatalf("mode %s task %d: err = %v", mode, id, err)
			}
		}
		w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "yta-" + string(mode)}
		if _, _, err := s.CreateMCPTaskComment(ctx, task, quoted, w); !errors.Is(err, ErrYTCommentUnsendable) {
			t.Fatalf("mode %s mcp: err = %v", mode, err)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM task_comments`); n != comments {
		t.Fatalf("%d comments after refusals, want %d", n, comments)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != ops {
		t.Fatalf("%d ops after refusals, want %d", n, ops)
	}
	// A sendable comment is unaffected.
	s.SetYTCommentsMode(YTCommentsCanary)
	c, err := s.CreateTaskComment(ctx, task, "worker", "sendable")
	if err != nil {
		t.Fatal(err)
	}
	if op, ok := ytcOpFor(t, s, c.ID); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("sendable under youtrack: op %+v %v", op, ok)
	}

	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "operator", "rollback"); err != nil {
		t.Fatal(err)
	}
	assertBornRejected()
}
