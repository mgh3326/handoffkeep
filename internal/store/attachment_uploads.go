package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// Upload states. A row moves pending -> completing -> done or refused; a
// failed attempt that may be retried moves completing -> pending.
const (
	AttachmentUploadPending    = "pending"
	AttachmentUploadCompleting = "completing"
	AttachmentUploadDone       = "done"
	AttachmentUploadRefused    = "refused"
)

// AttachmentUpload is one direct upload: the declared bytes, the staging object
// the client PUTs to, and the state of the completion. StagingDeletedAt is zero
// while the staging budget still charges the upload; it is set only once the
// staging object is confirmed absent and its presigned URL has expired.
type AttachmentUpload struct {
	ID               string
	StagingKey       string
	Name             string
	MIME             string
	SizeBytes        int64
	SHA256           string
	RefKind          string
	RefID            string
	CreatedBy        string
	Status           string
	Reason           string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	UpdatedAt        time.Time
	StagingDeletedAt time.Time
}

var attachmentUploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// uploadColumns is the one column list for reading an upload row.
const uploadColumns = `id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at,staging_deleted_at`

func scanUpload(row pgx.Row) (AttachmentUpload, error) {
	var u AttachmentUpload
	var released *time.Time
	err := row.Scan(&u.ID, &u.StagingKey, &u.Name, &u.MIME, &u.SizeBytes, &u.SHA256, &u.RefKind, &u.RefID, &u.CreatedBy, &u.Status, &u.Reason, &u.CreatedAt, &u.ExpiresAt, &u.UpdatedAt, &released)
	if released != nil {
		u.StagingDeletedAt = *released
	}
	return u, err
}

// CreateAttachmentUpload records a begun upload in the pending state without
// the staging budget check. Direct uploads use ReserveAttachmentUpload.
func (s *Store) CreateAttachmentUpload(ctx context.Context, u AttachmentUpload) error {
	if !attachmentUploadIDPattern.MatchString(u.ID) {
		return errors.New("invalid attachment upload id")
	}
	_, e := s.pool.Exec(ctx, `INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending','',$10,$11,$10)`,
		u.ID, u.StagingKey, u.Name, u.MIME, u.SizeBytes, u.SHA256, u.RefKind, u.RefID, u.CreatedBy, u.CreatedAt, u.ExpiresAt)
	return e
}

// GetAttachmentUpload reads one upload by id; found is false when none exists.
func (s *Store) GetAttachmentUpload(ctx context.Context, id string) (AttachmentUpload, bool, error) {
	if !attachmentUploadIDPattern.MatchString(id) {
		return AttachmentUpload{}, false, nil
	}
	u, e := scanUpload(s.pool.QueryRow(ctx, `SELECT `+uploadColumns+` FROM attachment_uploads WHERE id=$1`, id))
	if errors.Is(e, pgx.ErrNoRows) {
		return AttachmentUpload{}, false, nil
	}
	if e != nil {
		return AttachmentUpload{}, false, e
	}
	return u, true, nil
}

// TransitionAttachmentUpload moves one upload from one state to another only if
// it is still in from. It reports whether this call made the move, so two
// concurrent completions cannot both claim the same upload. updated_at records
// when the state last changed; a completion uses it as its lease start.
func (s *Store) TransitionAttachmentUpload(ctx context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	tag, e := s.pool.Exec(ctx, `UPDATE attachment_uploads SET status=$3, reason=$4, updated_at=$5 WHERE id=$1 AND status=$2`, id, from, to, reason, now)
	if e != nil {
		return false, e
	}
	return tag.RowsAffected() == 1, nil
}

// MarkAttachmentUploadStagingDeleted releases an upload's staging charge. The
// caller must have confirmed the staging object is absent and that no presigned
// PUT for it can still succeed.
func (s *Store) MarkAttachmentUploadStagingDeleted(ctx context.Context, id string, now time.Time) error {
	_, e := s.pool.Exec(ctx, `UPDATE attachment_uploads SET staging_deleted_at=$2 WHERE id=$1 AND staging_deleted_at IS NULL`, id, now)
	return e
}

// ListUnreleasedAttachmentUploads returns up to limit uploads still charged to
// the budget whose creation is before createdBefore, oldest first. The sweep
// reconciles them against the object store.
func (s *Store) ListUnreleasedAttachmentUploads(ctx context.Context, createdBefore time.Time, limit int) ([]AttachmentUpload, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+uploadColumns+` FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_at < $1 ORDER BY created_at, id LIMIT $2`, createdBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttachmentUpload
	for rows.Next() {
		u, e := scanUpload(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ErrAttachmentStagingCap means the staged bytes outstanding would exceed the
// staging budget, globally or for one client. It is refused before any upload
// row or URL is created.
var ErrAttachmentStagingCap = errors.New("attachment_staging_cap")

// ReserveAttachmentUpload records a begun upload only if the bytes it declares,
// added to the bytes still charged to the budget, stay within stagingCap, and
// the bytes charged to its creator stay within clientCap. Charged means the
// staging object may still exist: a row stops counting only after
// MarkAttachmentUploadStagingDeleted, not when it is completed or refused. The
// check and the insert run under one advisory lock, so concurrent begins cannot
// both pass it.
func (s *Store) ReserveAttachmentUpload(ctx context.Context, u AttachmentUpload, stagingCap, clientCap int64) error {
	if !attachmentUploadIDPattern.MatchString(u.ID) {
		return errors.New("invalid attachment upload id")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('attachment_staging_budget', 0))`); err != nil {
		return err
	}
	var used, usedByClient int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0)::BIGINT FROM attachment_uploads WHERE staging_deleted_at IS NULL`).Scan(&used); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0)::BIGINT FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_by=$1`, u.CreatedBy).Scan(&usedByClient); err != nil {
		return err
	}
	if used+u.SizeBytes > stagingCap || usedByClient+u.SizeBytes > clientCap {
		return ErrAttachmentStagingCap
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending','',$10,$11,$10)`,
		u.ID, u.StagingKey, u.Name, u.MIME, u.SizeBytes, u.SHA256, u.RefKind, u.RefID, u.CreatedBy, u.CreatedAt, u.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
