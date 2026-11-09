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
// the client PUTs to, and the state of the completion.
type AttachmentUpload struct {
	ID         string
	StagingKey string
	Name       string
	MIME       string
	SizeBytes  int64
	SHA256     string
	RefKind    string
	RefID      string
	CreatedBy  string
	Status     string
	Reason     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UpdatedAt  time.Time
}

var attachmentUploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// CreateAttachmentUpload records a begun upload in the pending state. The
// caller validates the declaration; the table re-checks the id, sha and ref.
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
	var u AttachmentUpload
	e := s.pool.QueryRow(ctx, `SELECT id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at FROM attachment_uploads WHERE id=$1`, id).
		Scan(&u.ID, &u.StagingKey, &u.Name, &u.MIME, &u.SizeBytes, &u.SHA256, &u.RefKind, &u.RefID, &u.CreatedBy, &u.Status, &u.Reason, &u.CreatedAt, &u.ExpiresAt, &u.UpdatedAt)
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
// concurrent completions cannot both claim the same upload.
func (s *Store) TransitionAttachmentUpload(ctx context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	tag, e := s.pool.Exec(ctx, `UPDATE attachment_uploads SET status=$3, reason=$4, updated_at=$5 WHERE id=$1 AND status=$2`, id, from, to, reason, now)
	if e != nil {
		return false, e
	}
	return tag.RowsAffected() == 1, nil
}

// ErrAttachmentStagingCap means the staged bytes outstanding would exceed the
// staging budget. It is refused before any upload row or URL is created.
var ErrAttachmentStagingCap = errors.New("attachment_staging_cap")

// ReserveAttachmentUpload records a begun upload only if the bytes it declares,
// added to the bytes of uploads still staged since the given time, stay within
// stagingCap. The check and the insert run under one advisory lock, so
// concurrent begins cannot both pass the check. An upload stops counting once
// it is done or refused: its staging object is then gone or about to be.
func (s *Store) ReserveAttachmentUpload(ctx context.Context, u AttachmentUpload, stagingCap int64, since time.Time) error {
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
	var used int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0)::BIGINT FROM attachment_uploads WHERE status IN ('pending','completing') AND created_at > $1`, since).Scan(&used); err != nil {
		return err
	}
	if used+u.SizeBytes > stagingCap {
		return ErrAttachmentStagingCap
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending','',$10,$11,$10)`,
		u.ID, u.StagingKey, u.Name, u.MIME, u.SizeBytes, u.SHA256, u.RefKind, u.RefID, u.CreatedBy, u.CreatedAt, u.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
