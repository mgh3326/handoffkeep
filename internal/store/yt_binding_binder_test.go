package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The automatic binder's durable state (hk 4492, design/2026-11-13/yt-comment-binding-lifecycle
// §6.6, §6.9): the comment cursor, the intents it records, their backoff, and
// the publication with an evidence watermark and origin auto_verified
// (invariants I7, I8, I11).

func ybbIntent(t *testing.T, s *Store, task int64) (YTBindingIntent, bool) {
	t.Helper()
	var in YTBindingIntent
	var state string
	err := s.pool.QueryRow(t.Context(), `SELECT task_id, tenant, state, reason, attempts, last_error, next_attempt_at, recorded_at
		FROM yt_binding_intents WHERE task_id=$1`, task).
		Scan(&in.TaskID, &in.Tenant, &state, &in.Reason, &in.Attempts, &in.LastError, &in.NextAttemptAt, &in.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTBindingIntent{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	in.State = YTBindingIntentState(state)
	return in, true
}

func ybbNoIntent(t *testing.T, s *Store, name string, task int64) {
	t.Helper()
	if in, ok := ybbIntent(t, s, task); ok {
		t.Errorf("%s: task %d has intent %+v", name, task, in)
	}
}

// The scan queues one intent per unbound task it sees, advances the cursor,
// and leaves a bound task and one whose issue_create is pending alone. It
// never resets an existing row, so a backoff survives a second pass.
func TestScanYTBindingCandidates(t *testing.T) {
	s := schemaStore(t, "ybb_scan")
	ctx := t.Context()
	ytbMapped(t, s)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	first := ytcTask(t, s, ytbProject)
	second := ytcTask(t, s, ytbProject)
	bound := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(bound))
	pending := ytcTask(t, s, ytbProject)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_issue_intents(task_id, tenant, recorded_at) VALUES($1,$2,$3)`, pending, ytcTenant, now); err != nil {
		t.Fatal(err)
	}
	// A task whose issue_create op is still unresolved publishes its binding
	// when the op applies: the binder leaves it alone too.
	inFlight := ytcTask(t, s, ytbProject)
	ybhOp(t, s, YTIssuePrincipal, ytwrite.KindIssueCreate, inFlight, "ytb-issue")
	ytcuRawComment(t, s, first, "worker")
	ytcuRawComment(t, s, bound, "worker")
	ytcuRawComment(t, s, second, "worker")
	ytcuRawComment(t, s, pending, "worker")
	ytcuRawComment(t, s, inFlight, "worker")

	scan, err := s.ScanYTBindingCandidates(ctx, ytcTenant, 200, now)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Comments != 5 || scan.Tasks != 2 {
		t.Fatalf("scan %+v, want 5 comments and 2 intents", scan)
	}
	for _, task := range []int64{first, second} {
		in, ok := ybbIntent(t, s, task)
		if !ok || in.State != YTBindingIntentPending || in.Tenant != ytcTenant || in.Attempts != 0 ||
			!in.NextAttemptAt.Equal(now) || !in.RecordedAt.Equal(now) {
			t.Fatalf("task %d intent %+v %v", task, in, ok)
		}
	}
	ybbNoIntent(t, s, "bound task", bound)
	ybbNoIntent(t, s, "pending issue task", pending)
	ybbNoIntent(t, s, "in-flight issue task", inFlight)

	// The cursor moved: nothing is read or queued twice.
	if again, err := s.ScanYTBindingCandidates(ctx, ytcTenant, 200, now.Add(time.Minute)); err != nil || again.Comments != 0 || again.Tasks != 0 {
		t.Fatalf("second scan %+v %v, want nothing", again, err)
	}
	// A refusal's backoff survives a later comment on the same task.
	if err := s.RetryYTBindingIntent(ctx, first, YTBindingIntentRefused, "no_issue", "no issue", now); err != nil {
		t.Fatal(err)
	}
	before, _ := ybbIntent(t, s, first)
	ytcuRawComment(t, s, first, "worker")
	after, err := s.ScanYTBindingCandidates(ctx, ytcTenant, 200, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if after.Comments != 1 || after.Tasks != 0 {
		t.Fatalf("scan after a later comment %+v, want the comment read and no new intent", after)
	}
	if got, _ := ybbIntent(t, s, first); got.State != YTBindingIntentRefused || got.Attempts != 1 || !got.NextAttemptAt.Equal(before.NextAttemptAt) {
		t.Fatalf("the re-scan reset the intent: %+v, want %+v", got, before)
	}
}

func TestYTBindingIntentBackoffSchedule(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0: time.Hour, 1: time.Hour, 2: 6 * time.Hour, 3: 24 * time.Hour, 4: 7 * 24 * time.Hour, 8: 7 * 24 * time.Hour,
	} {
		if got := ytBindingIntentBackoff(attempts); got != want {
			t.Errorf("attempts %d: %s, want %s", attempts, got, want)
		}
	}
}

// A refused or failed intent waits 1 h, 6 h, 24 h, then a week, and its
// attempt count is capped at 8; a due read sees it only when the wait is over.
func TestYTBindingIntentBackoff(t *testing.T) {
	s := schemaStore(t, "ybb_backoff")
	ctx := t.Context()
	ytbMapped(t, s)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	task := ytcTask(t, s, ytbProject)
	ytcuRawComment(t, s, task, "worker")
	if _, err := s.ScanYTBindingCandidates(ctx, ytcTenant, 200, now); err != nil {
		t.Fatal(err)
	}
	if due, err := s.YTBindingIntentsDue(ctx, ytcTenant, now, 20); err != nil || len(due) != 1 || due[0].TaskID != task {
		t.Fatalf("a fresh intent is not due: %+v %v", due, err)
	}
	want := []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}
	at := now
	for i, wait := range want {
		if err := s.RetryYTBindingIntent(ctx, task, YTBindingIntentRefused, "no_issue", "no issue", at); err != nil {
			t.Fatal(err)
		}
		in, _ := ybbIntent(t, s, task)
		if in.Attempts != i+1 || !in.NextAttemptAt.Equal(at.Add(wait)) {
			t.Fatalf("attempt %d: %+v, want attempts %d and next %s", i+1, in, i+1, at.Add(wait))
		}
		if due, err := s.YTBindingIntentsDue(ctx, ytcTenant, at.Add(wait-time.Second), 20); err != nil || len(due) != 0 {
			t.Fatalf("attempt %d: an intent came due early: %+v %v", i+1, due, err)
		}
		if due, err := s.YTBindingIntentsDue(ctx, ytcTenant, at.Add(wait), 20); err != nil || len(due) != 1 {
			t.Fatalf("attempt %d: the intent did not come due: %+v %v", i+1, due, err)
		}
		at = at.Add(wait)
	}
	for range 5 {
		if err := s.RetryYTBindingIntent(ctx, task, YTBindingIntentError, "", "boom", at); err != nil {
			t.Fatal(err)
		}
	}
	if in, _ := ybbIntent(t, s, task); in.Attempts != 8 || in.State != YTBindingIntentError || !in.NextAttemptAt.Equal(at.Add(7*24*time.Hour)) {
		t.Fatalf("after 9 failures: %+v, want 8 attempts capped at one week", in)
	}
	// Only the tenant's own intents are read, and a terminal one never.
	if due, err := s.YTBindingIntentsDue(ctx, "other.example", at.Add(8*24*time.Hour), 20); err != nil || len(due) != 0 {
		t.Fatalf("another tenant read the intent: %+v %v", due, err)
	}
	if err := s.ExcludeYTBindingIntent(ctx, task, "excluded", at); err != nil {
		t.Fatal(err)
	}
	if in, _ := ybbIntent(t, s, task); in.State != YTBindingIntentExcluded {
		t.Fatalf("excluded intent %+v", in)
	}
	if due, err := s.YTBindingIntentsDue(ctx, ytcTenant, at.Add(30*24*time.Hour), 20); err != nil || len(due) != 0 {
		t.Fatalf("an excluded intent came due: %+v %v", due, err)
	}
	if err := s.DropYTBindingIntent(ctx, task); err != nil {
		t.Fatal(err)
	}
	ybbNoIntent(t, s, "after a drop", task)
}

// The binder's publication carries the evidence watermark, not the task's
// highest comment id, and records origin auto_verified and its actor (I8).
func TestAddYTTaskBindingFromKeepsWatermark(t *testing.T) {
	s := schemaStore(t, "ybb_from")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	first := ytcuRawComment(t, s, task, "worker")
	ytcuRawComment(t, s, task, "worker")
	b := ytbBinding(task)
	verified, err := s.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AddYTTaskBindingFrom(ctx, b, verified, first.ID, YTBindingAutoVerified, ytBindTestActor)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommentsFrom != first.ID || got.Origin != YTBindingAutoVerified {
		t.Fatalf("published %+v, want comments_from %d and origin %s", got, first.ID, YTBindingAutoVerified)
	}
	want := []ybhRow{{Action: "bound", Origin: YTBindingAutoVerified, Actor: ytBindTestActor, Issue: b.IssueID}}
	if hist := ybhHistory(t, s, task); len(hist) != 1 || hist[0] != want[0] {
		t.Fatalf("history %+v, want %+v", hist, want)
	}
	// A watermark of 0 is real: the issue holds none of the task's comments.
	other := ytcTask(t, s, ytbProject)
	ytcuRawComment(t, s, other, "worker")
	verified, err = s.YTCanaryCandidate(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.AddYTTaskBindingFrom(ctx, ytbBinding(other), verified, 0, YTBindingAutoVerified, ytBindTestActor); err != nil || got.CommentsFrom != 0 {
		t.Fatalf("zero watermark: %+v %v", got, err)
	}
	// A negative watermark and an unknown origin are refused.
	if _, err := s.AddYTTaskBindingFrom(ctx, ytbBinding(ytcTask(t, s, ytbProject)), YTCanaryCandidate{}, -1, YTBindingAutoVerified, ytBindTestActor); err == nil {
		t.Fatal("a negative watermark was accepted")
	}
	verified, err = s.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTTaskBindingFrom(ctx, b, verified, 0, "auto", ytBindTestActor); err == nil {
		t.Fatal("an unknown origin was accepted")
	}
}

const ytBindTestActor = "operator:auto-bind"

// An operator unbind is visible to the binder until something binds the task
// again (I7).
func TestYTBindingExcluded(t *testing.T) {
	s := schemaStore(t, "ybb_excluded")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	b := ytbBinding(task)
	ytbBind(t, s, b)
	if excluded, err := s.YTBindingExcluded(ctx, task); err != nil || excluded {
		t.Fatalf("a bound task reads excluded=%v %v", excluded, err)
	}
	if _, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if excluded, err := s.YTBindingExcluded(ctx, task); err != nil || !excluded {
		t.Fatalf("after the unbind: excluded=%v %v", excluded, err)
	}
	ytbBind(t, s, b)
	if excluded, err := s.YTBindingExcluded(ctx, task); err != nil || excluded {
		t.Fatalf("after an explicit rebind: excluded=%v %v", excluded, err)
	}
	if id, err := s.YTHighestCommentID(ctx, task); err != nil || id != 0 {
		t.Fatalf("highest comment id %d %v, want 0", id, err)
	}
}
