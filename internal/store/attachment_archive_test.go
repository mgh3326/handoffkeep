package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func archiveTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	s := schemaStore(t, "attachment_archive")
	return s, s.pool
}

// archiveTask inserts one task, and — when closedAt is set — the event that
// moved it into state at that time. The close time comes from task_events, so
// the seed puts it there.
func archiveTask(t *testing.T, pool *pgxpool.Pool, state string, closedAt *time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at) VALUES('archive-test',$1,'implement',$2,'tester',now()-interval '40 days',now(),now()) RETURNING id`, "archive "+state, state).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if closedAt != nil {
		if _, err := pool.Exec(t.Context(), `INSERT INTO task_events(task_id,"from","to","by","at") VALUES($1,'developing',$2,'tester',$3)`, id, state, *closedAt); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func archiveAttachment(t *testing.T, s *Store, n int, refKind, refID string) string {
	t.Helper()
	sha := fmt.Sprintf("%064x", n)
	x := Attachment{SHA256: sha, SizeBytes: 10, MIME: "text/plain", OriginalName: fmt.Sprintf("%d.txt", n), CreatedBy: "node", RefKind: refKind, RefID: refID}
	if _, _, err := s.PutAttachment(t.Context(), x, 1<<40, 1<<40); err != nil {
		t.Fatalf("put %s (%s:%s): %v", sha, refKind, refID, err)
	}
	return sha
}

func archiveTaskRef(id int64) string { return strconv.FormatInt(id, 10) }

// TestAttachmentArchiveCandidateRules is the design's rule as a table: open
// task, fresh close, another ref kind, a loose document and an open decision
// each exclude a blob; the storage task is the lowest referring id.
func TestAttachmentArchiveCandidateRules(t *testing.T) {
	s, pool := archiveTestStore(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	oldClose := now.Add(-30 * 24 * time.Hour)
	recentClose := now.Add(-3 * 24 * time.Hour)
	eightDays := now.Add(-8 * 24 * time.Hour)

	openTask := archiveTask(t, pool, StateDeveloping, nil)
	recentTask := archiveTask(t, pool, StateMerged, &recentClose)
	oldTask := archiveTask(t, pool, StateMerged, &oldClose)
	droppedTask := archiveTask(t, pool, StateDropped, &eightDays)

	openSha := archiveAttachment(t, s, 1, "task", archiveTaskRef(openTask))
	recentSha := archiveAttachment(t, s, 2, "task", archiveTaskRef(recentTask))
	eligibleSha := archiveAttachment(t, s, 3, "task", archiveTaskRef(oldTask))
	sharedSha := archiveAttachment(t, s, 4, "task", archiveTaskRef(oldTask))
	archiveAttachment(t, s, 4, "task", archiveTaskRef(droppedTask))
	sharedOpenSha := archiveAttachment(t, s, 5, "task", archiveTaskRef(oldTask))
	archiveAttachment(t, s, 5, "task", archiveTaskRef(openTask))
	memorySha := archiveAttachment(t, s, 6, "memory", "agent/name")
	decisionSha := archiveAttachment(t, s, 7, "task", archiveTaskRef(oldTask))
	docTaskSha := archiveAttachment(t, s, 8, "document", "archive/test-doc")
	docLooseSha := archiveAttachment(t, s, 9, "document", "archive/loose-doc")

	if _, err := pool.Exec(t.Context(), `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id) VALUES('archive/test-doc','note','','','body','x','tester',now(),now(),$1),('archive/loose-doc','note','','','body','x','tester',now(),now(),NULL)`, oldTask); err != nil {
		t.Fatal(err)
	}
	options := fmt.Sprintf(`{"options":[{"key":"A","label":"a","attachments":[{"sha256":%q}]}],"allow_free":true}`, decisionSha)
	if _, err := pool.Exec(t.Context(), `INSERT INTO decision_requests(source,source_ref,task_id,job_id,lane,question,options,recommended,default_action,status,created_at,updated_at) VALUES('lane_event','archive-open-decision',NULL,'','archive-test','which?',$1::jsonb,'','','open',now(),now())`, options); err != nil {
		t.Fatal(err)
	}

	xs, next, err := s.ListAttachmentArchiveCandidates(t.Context(), 100, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("next=%q on a complete page", next)
	}
	got := map[string]AttachmentArchiveCandidate{}
	for _, x := range xs {
		got[x.SHA256] = x
	}
	for _, bad := range []string{openSha, recentSha, sharedOpenSha, memorySha, decisionSha, docLooseSha} {
		if _, ok := got[bad]; ok {
			t.Fatalf("%s must not be a candidate", bad)
		}
	}
	if len(xs) != 3 {
		t.Fatalf("candidates=%d (%v), want 3", len(xs), xs)
	}
	if _, ok := got[eligibleSha]; !ok {
		t.Fatalf("%s missing", eligibleSha)
	}
	shared, ok := got[sharedSha]
	if !ok {
		t.Fatalf("%s missing", sharedSha)
	}
	if shared.TaskID != oldTask {
		t.Fatalf("storage task=%d, want the lowest id %d", shared.TaskID, oldTask)
	}
	if len(shared.Tasks) != 2 || shared.Tasks[0].ID != oldTask || shared.Tasks[1].ID != droppedTask {
		t.Fatalf("tasks=%+v", shared.Tasks)
	}
	if !shared.Tasks[0].ClosedAt.Equal(oldClose) || !shared.Tasks[1].ClosedAt.Equal(eightDays) {
		t.Fatalf("close times %+v", shared.Tasks)
	}
	if _, ok := got[docTaskSha]; !ok {
		t.Fatalf("a document belonging to a closed task must count, %s missing", docTaskSha)
	}
	for i := 1; i < len(xs); i++ {
		if xs[i-1].SHA256 >= xs[i].SHA256 {
			t.Fatalf("not sha ordered: %v", xs)
		}
	}

	// A full page carries the last sha as the cursor; the next page resumes.
	first, cursor, err := s.ListAttachmentArchiveCandidates(t.Context(), 2, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || cursor != first[1].SHA256 {
		t.Fatalf("first page=%v cursor=%q", first, cursor)
	}
	second, cursor2, err := s.ListAttachmentArchiveCandidates(t.Context(), 2, cursor, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || cursor2 != "" || second[0].SHA256 == first[0].SHA256 {
		t.Fatalf("second page=%v cursor=%q", second, cursor2)
	}

	// An archived blob leaves the candidate list.
	if _, err := s.RecordAttachmentArchive(t.Context(), eligibleSha, "fleet-artifacts/archive/tasks/1/attachments/x", int64(10), now); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.ListAttachmentArchiveCandidates(t.Context(), 100, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("archived blob still listed: %v", after)
	}
	for _, x := range after {
		if x.SHA256 == eligibleSha {
			t.Fatal("archived blob still listed")
		}
	}
}

// TestAttachmentArchiveRecordChecksAndIdempotency covers the record's refusals
// and its recorded result.
func TestAttachmentArchiveRecordChecksAndIdempotency(t *testing.T) {
	s, pool := archiveTestStore(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	closed := now.Add(-30 * 24 * time.Hour)

	closedTask := archiveTask(t, pool, StateMerged, &closed)
	openTask := archiveTask(t, pool, StateDeveloping, nil)
	sha := archiveAttachment(t, s, 1, "task", archiveTaskRef(closedTask))
	openSha := archiveAttachment(t, s, 2, "task", archiveTaskRef(openTask))
	location := fmt.Sprintf("fleet-artifacts/archive/tasks/%d/attachments/%s-1.txt", closedTask, sha)
	unknown := strings.Repeat("f", 64)

	if _, err := s.RecordAttachmentArchive(t.Context(), openSha, location, 10, now); !errors.Is(err, ErrAttachmentNotArchivable) {
		t.Fatalf("open task err=%v, want ErrAttachmentNotArchivable", err)
	}
	if _, err := s.RecordAttachmentArchive(t.Context(), unknown, location, 10, now); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unknown sha err=%v, want ErrAttachmentNotFound", err)
	}
	if _, err := s.RecordAttachmentArchive(t.Context(), sha, location, 11, now); !errors.Is(err, ErrAttachmentArchiveMismatch) {
		t.Fatalf("size mismatch err=%v", err)
	}
	rec, err := s.RecordAttachmentArchive(t.Context(), sha, location, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ArchivedAt.Equal(now) || rec.Location != location || rec.HotDeleted {
		t.Fatalf("record %+v", rec)
	}
	again, err := s.RecordAttachmentArchive(t.Context(), sha, location, 10, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.ArchivedAt.Equal(rec.ArchivedAt) {
		t.Fatalf("idempotent repeat moved archived_at: %+v vs %+v", again, rec)
	}
	if _, err := s.RecordAttachmentArchive(t.Context(), sha, location+"-other", 10, now); !errors.Is(err, ErrAttachmentArchiveMismatch) {
		t.Fatalf("different location err=%v", err)
	}

	row, found, err := s.GetAttachment(t.Context(), sha)
	if err != nil || !found {
		t.Fatalf("get found=%t err=%v", found, err)
	}
	if row.ArchivedAt == nil || !row.ArchivedAt.Equal(now) || row.ArchiveLocation != location {
		t.Fatalf("stored row %+v", row)
	}

	// An archived blob takes no new ref. The put takes the same row lock the
	// archive record takes, so a ref to open work either lands before the
	// candidate check or is refused here — it can never commit behind it.
	if _, _, err := s.PutAttachment(t.Context(), Attachment{SHA256: sha, SizeBytes: 10, MIME: "text/plain", OriginalName: "1.txt", CreatedBy: "node", RefKind: "task", RefID: archiveTaskRef(openTask)}, 1<<40, 1<<40); !errors.Is(err, ErrAttachmentArchived) {
		t.Fatalf("put on an archived blob err=%v, want ErrAttachmentArchived", err)
	}

	if err := s.MarkAttachmentHotDeleted(t.Context(), sha, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAttachmentHotDeleted(t.Context(), sha, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	row, _, err = s.GetAttachment(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if row.HotDeletedAt == nil || !row.HotDeletedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("hot_deleted_at=%v, want the first mark", row.HotDeletedAt)
	}
}

// The candidate cutoff is now minus the grace in force, and SetAttachmentArchiveGrace
// replaces the 7-day default (HANDOFFKEEP_ATTACH_ARCHIVE_GRACE at serve start).
func TestAttachmentArchiveCutoffHonoursSetGrace(t *testing.T) {
	t.Cleanup(func() { SetAttachmentArchiveGrace(AttachmentArchiveGrace) })
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if got, want := attachmentArchiveCutoff(now), now.Add(-7*24*time.Hour); !got.Equal(want) {
		t.Fatalf("default cutoff = %v, want %v", got, want)
	}
	SetAttachmentArchiveGrace(24 * time.Hour)
	if got, want := attachmentArchiveCutoff(now), now.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("set cutoff = %v, want %v", got, want)
	}
}
