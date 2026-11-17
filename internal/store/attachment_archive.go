package store

import (
	"context"
	"errors"
	"time"
)

// AttachmentArchiveGrace is how long every referring task must have been
// closed before an attachment is archivable (design/2026-11-16/
// hk-attachment-archive, decision/2026-11-09/hk-file-gateway §4). The clock
// and the cutoff are passed in, so a reader and the record see one now.
const AttachmentArchiveGrace = 7 * 24 * time.Hour

// AttachmentArchiveCandidateTask is one task that refers to an attachment.
// ClosedAt is the task's last transition into a settled state, read from
// task_events: the state column alone cannot say when it got there.
type AttachmentArchiveCandidateTask struct {
	ID       int64     `json:"id"`
	State    string    `json:"state"`
	ClosedAt time.Time `json:"closed_at"`
}

// AttachmentArchiveCandidate is one blob the archiver may copy out: every task
// that refers to it is closed and the latest close is at least
// AttachmentArchiveGrace old. TaskID is the storage task, the lowest-id
// referring task whose archive directory holds the file.
type AttachmentArchiveCandidate struct {
	SHA256       string                           `json:"sha256"`
	SizeBytes    int64                            `json:"size_bytes"`
	MIME         string                           `json:"mime"`
	OriginalName string                           `json:"original_name"`
	TaskID       int64                            `json:"task_id"`
	Tasks        []AttachmentArchiveCandidateTask `json:"tasks"`
}

// AttachmentArchive is the recorded result of one archive call. HotDeleted is
// false in record mode, and stays false while a delete is still owed.
type AttachmentArchive struct {
	ArchivedAt time.Time `json:"archived_at"`
	Location   string    `json:"location"`
	HotDeleted bool      `json:"hot_deleted"`
}

var (
	// ErrAttachmentNotArchivable means the attachment does not meet the
	// archive rule right now (a task is open or the grace period has not
	// passed, another ref kind exists, or an open decision names it).
	ErrAttachmentNotArchivable = errors.New("attachment_not_archivable")
	// ErrAttachmentArchiveMismatch means the declared size, sha or location
	// disagrees with the stored record.
	ErrAttachmentArchiveMismatch = errors.New("attachment_archive_mismatch")
	// ErrAttachmentNotFound means no attachment row holds the address. Its
	// text is the wire code the download route already answers.
	ErrAttachmentNotFound = errors.New("attachment_not_found")
)

// attachmentArchiveCandidatesSQL selects the archivable blobs. $1 is now, $2
// the grace cutoff, $3 the after cursor (” for none), $4 one sha (” for
// all), $5 the limit.
//
// A ref is a task ref (attachment_refs.ref_kind 'task') or a document ref
// whose document belongs to a task; the document's task is the referring
// task. Any other ref excludes the blob, and so does a document ref without a
// task. Every referring task must be settled and carry a last close no later
// than the cutoff; the close comes from task_events, not tasks.completed_at,
// so reopens and re-closes are judged by the last one. A live decision whose
// options name the sha excludes it (hk 4788).
const attachmentArchiveCandidatesSQL = `
WITH ref_rows AS (
	SELECT a.sha256, a.size_bytes, a.mime, a.original_name,
	       CASE WHEN r.ref_kind='task' AND r.ref_id ~ '^[1-9][0-9]*$' THEN r.ref_id::bigint
	            WHEN r.ref_kind='document' THEN d.task_id END AS task_id,
	       (r.ref_kind='task' AND r.ref_id ~ '^[1-9][0-9]*$')
	        OR (r.ref_kind='document' AND d.task_id IS NOT NULL) AS task_ref
	FROM attachments a
	JOIN attachment_refs r ON r.sha256=a.sha256
	LEFT JOIN documents d ON r.ref_kind='document' AND d.key=r.ref_id
	WHERE a.archived_at IS NULL
), per_sha AS (
	SELECT sha256, min(size_bytes) AS size_bytes, min(mime) AS mime,
	       min(original_name) AS original_name,
	       bool_and(task_ref) AS all_task_refs,
	       array_agg(DISTINCT task_id) FILTER (WHERE task_id IS NOT NULL) AS task_ids
	FROM ref_rows
	GROUP BY sha256
), closed AS (
	SELECT p.sha256, p.size_bytes, p.mime, p.original_name, t.id AS task_id, t.state,
	       (SELECT max(e."at") FROM task_events e
	         WHERE e.task_id=t.id AND e."to" IN (` + settledStatesSQL + `)) AS closed_at
	FROM per_sha p
	CROSS JOIN LATERAL unnest(p.task_ids) AS u(task_id)
	JOIN tasks t ON t.id=u.task_id
	WHERE p.all_task_refs
), eligible AS (
	SELECT sha256, size_bytes, mime, original_name,
	       min(task_id) AS task_id,
	       jsonb_agg(jsonb_build_object('id', task_id, 'state', state, 'closed_at', closed_at) ORDER BY task_id) AS tasks,
	       max(closed_at) AS last_close
	FROM closed
	GROUP BY sha256, size_bytes, mime, original_name
)
SELECT sha256, size_bytes, mime, original_name, task_id, tasks
FROM eligible e
WHERE e.last_close IS NOT NULL AND e.last_close <= $2
  AND NOT EXISTS (SELECT 1 FROM closed c WHERE c.sha256=e.sha256
                   AND (c.closed_at IS NULL OR c.state NOT IN (` + settledStatesSQL + `)))
  AND NOT EXISTS (
	  SELECT 1 FROM decision_requests d
	  WHERE d.status='open' AND (d.expires_at IS NULL OR d.expires_at>$1)
	    AND EXISTS (
	      SELECT 1 FROM jsonb_array_elements(COALESCE(d.options->'options','[]'::jsonb)) o
	      CROSS JOIN LATERAL jsonb_array_elements(COALESCE(o->'attachments','[]'::jsonb)) att
	      WHERE att->>'sha256'=e.sha256))
  AND ($3='' OR e.sha256>$3)
  AND ($4='' OR e.sha256=$4)
ORDER BY e.sha256
LIMIT $5`

// ListAttachmentArchiveCandidates pages the archivable blobs by sha. next is
// the sha to pass as after for the following page, or "" when this page is
// the last.
func (s *Store) ListAttachmentArchiveCandidates(ctx context.Context, limit int, after string, now time.Time) ([]AttachmentArchiveCandidate, string, error) {
	if s != nil && s.dialect == mysqlDialect {
		return nil, "", ErrDialectUnsupported
	}

	if after != "" && !attachmentSHARE.MatchString(after) {
		return nil, "", errors.New("invalid attachment archive cursor")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return attachmentArchiveCandidates(ctx, s.db(), limit, after, "", now)
}

// attachmentArchiveCandidates reads one page with limit+1 rows so it can tell
// a full page from the last one.
func attachmentArchiveCandidates(ctx context.Context, q poolQuerier, limit int, after, sha string, now time.Time) ([]AttachmentArchiveCandidate, string, error) {
	now = now.UTC()
	rows, err := q.Query(ctx, attachmentArchiveCandidatesSQL, now, now.Add(-AttachmentArchiveGrace), after, sha, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []AttachmentArchiveCandidate{}
	for rows.Next() {
		var x AttachmentArchiveCandidate
		var tasks []byte
		if err = rows.Scan(&x.SHA256, &x.SizeBytes, &x.MIME, &x.OriginalName, &x.TaskID, &tasks); err != nil {
			return nil, "", err
		}
		x.Tasks = []AttachmentArchiveCandidateTask{}
		if _, err = scanJSONB(tasks, &x.Tasks); err != nil {
			return nil, "", err
		}
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) <= limit {
		return out, "", nil
	}
	out = out[:limit]
	return out, out[len(out)-1].SHA256, nil
}

// RecordAttachmentArchive records that the blob at sha was verified at
// location. It refuses a non-candidate, and a size that disagrees with the
// stored row. A repeat with the same location answers the recorded result
// (idempotent); a different location is a mismatch. The hot copy is not
// touched here: the caller deletes it after this commits.
func (s *Store) RecordAttachmentArchive(ctx context.Context, sha, location string, size int64, now time.Time) (AttachmentArchive, error) {
	if s != nil && s.dialect == mysqlDialect {
		return AttachmentArchive{}, ErrDialectUnsupported
	}

	if !attachmentSHARE.MatchString(sha) || location == "" || !validText(location, 2048) || size < 0 {
		return AttachmentArchive{}, errors.New("invalid attachment archive")
	}
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AttachmentArchive{}, err
	}
	defer tx.Rollback(ctx)
	var archivedAt, hotDeletedAt *time.Time
	var recorded string
	var sizeBytes int64
	err = tx.QueryRow(ctx, `SELECT archived_at, archive_location, hot_deleted_at, size_bytes FROM attachments WHERE sha256=$1 FOR UPDATE`, sha).
		Scan(&archivedAt, &recorded, &hotDeletedAt, &sizeBytes)
	if errors.Is(err, ErrNoRows) {
		return AttachmentArchive{}, ErrAttachmentNotFound
	}
	if err != nil {
		return AttachmentArchive{}, err
	}
	if sizeBytes != size {
		return AttachmentArchive{}, ErrAttachmentArchiveMismatch
	}
	if archivedAt != nil {
		if recorded != location {
			return AttachmentArchive{}, ErrAttachmentArchiveMismatch
		}
		return AttachmentArchive{ArchivedAt: archivedAt.UTC(), Location: recorded, HotDeleted: hotDeletedAt != nil}, nil
	}
	candidates, _, err := attachmentArchiveCandidates(ctx, pgTransaction{tx}, 1, "", sha, now)
	if err != nil {
		return AttachmentArchive{}, err
	}
	if len(candidates) == 0 {
		return AttachmentArchive{}, ErrAttachmentNotArchivable
	}
	if _, err = tx.Exec(ctx, `UPDATE attachments SET archived_at=$2, archive_location=$3 WHERE sha256=$1`, sha, now, location); err != nil {
		return AttachmentArchive{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AttachmentArchive{}, err
	}
	return AttachmentArchive{ArchivedAt: now, Location: location}, nil
}

// MarkAttachmentHotDeleted records that the object store copy of sha is gone.
// It is idempotent: a row already marked stays on its first timestamp.
func (s *Store) MarkAttachmentHotDeleted(ctx context.Context, sha string, now time.Time) error {
	if s != nil && s.dialect == mysqlDialect {
		return ErrDialectUnsupported
	}

	if !attachmentSHARE.MatchString(sha) {
		return errors.New("invalid attachment sha")
	}
	_, err := s.pool.Exec(ctx, `UPDATE attachments SET hot_deleted_at=$2 WHERE sha256=$1 AND hot_deleted_at IS NULL`, sha, now.UTC())
	return err
}
