package store

import (
	"context"
	"errors"
	"regexp"
	"time"
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
// ClaimNonce is the token the completion that moved the row to completing
// presents on every later transition; it is meaningful only while the row is
// completing, so a completion whose lease was reclaimed cannot move the row.
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
	ClaimNonce       string
}

var attachmentUploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// uploadColumns is the one column list for reading an upload row.
const uploadColumns = `id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at,staging_deleted_at,claim_nonce`

func scanUpload(row interface{ Scan(...any) error }) (AttachmentUpload, error) {
	var u AttachmentUpload
	var released *time.Time
	var nonce *string
	err := row.Scan(&u.ID, &u.StagingKey, &u.Name, &u.MIME, &u.SizeBytes, &u.SHA256, &u.RefKind, &u.RefID, &u.CreatedBy, &u.Status, &u.Reason, &u.CreatedAt, &u.ExpiresAt, &u.UpdatedAt, &released, &nonce)
	if released != nil {
		u.StagingDeletedAt = *released
	}
	if nonce != nil {
		u.ClaimNonce = *nonce
	}
	return u, err
}

// CreateAttachmentUpload records a begun upload in the pending state without
// the staging budget check. Direct uploads use ReserveAttachmentUpload.
func (s *Store) CreateAttachmentUpload(ctx context.Context, u AttachmentUpload) error {
	if !attachmentUploadIDPattern.MatchString(u.ID) {
		return errors.New("invalid attachment upload id")
	}
	_, e := s.db().Exec(ctx, s.q(`INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending','',$10,$11,$10)`, `INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'pending','',?,?,?)`), attachmentUploadInsertArgs(s.dialect, u)...)
	return e
}

// GetAttachmentUpload reads one upload by id; found is false when none exists.
func (s *Store) GetAttachmentUpload(ctx context.Context, id string) (AttachmentUpload, bool, error) {
	if !attachmentUploadIDPattern.MatchString(id) {
		return AttachmentUpload{}, false, nil
	}
	u, e := scanUpload(s.db().QueryRow(ctx, s.q(`SELECT `+uploadColumns+` FROM attachment_uploads WHERE id=$1`, `SELECT `+uploadColumns+` FROM attachment_uploads WHERE id=?`), id))
	if errors.Is(e, ErrNoRows) {
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
// the last claim or heartbeat while completing and when the state last changed
// otherwise; a completion uses it as its lease start. It is not claim-scoped:
// the lease reclaim and the pre-claim expiry refusal use it; every transition
// out of completing goes through the claim-scoped variants below.
func (s *Store) TransitionAttachmentUpload(ctx context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	tag, e := s.db().Exec(ctx, s.q(`UPDATE attachment_uploads SET status=$3, reason=$4, updated_at=$5 WHERE id=$1 AND status=$2`, `UPDATE attachment_uploads SET status=?,reason=?,updated_at=? WHERE id=? AND status=?`), uploadTransitionArgs(s.dialect, id, from, to, reason, now)...)
	if e != nil {
		return false, e
	}
	return tag == 1, nil
}

func uploadTransitionArgs(d storeDialect, id, from, to, reason string, now time.Time) []any {
	if d == mysqlDialect {
		return []any{to, reason, now, id, from}
	}
	return []any{id, from, to, reason, now}
}

// ClaimAttachmentUpload moves a pending upload to completing and stamps the
// caller's nonce as its claim. Only one caller can win because the update is
// conditional on status='pending'.
func (s *Store) ClaimAttachmentUpload(ctx context.Context, id, nonce string, now time.Time) (bool, error) {
	tag, e := s.db().Exec(ctx, s.q(`UPDATE attachment_uploads SET status='completing', claim_nonce=$2, reason='', updated_at=$3 WHERE id=$1 AND status='pending'`, `UPDATE attachment_uploads SET status='completing',claim_nonce=?,reason='',updated_at=? WHERE id=? AND status='pending'`), uploadClaimArgs(s.dialect, id, nonce, now)...)
	if e != nil {
		return false, e
	}
	return tag == 1, nil
}
func uploadClaimArgs(d storeDialect, id, nonce string, now time.Time) []any {
	if d == mysqlDialect {
		return []any{nonce, now, id}
	}
	return []any{id, nonce, now}
}

// TransitionAttachmentUploadClaim moves a completing upload to to only while
// the caller still holds its claim nonce, and consumes the claim. A completion
// whose lease was reclaimed finds its nonce replaced (or the row already
// final) and cannot move the row.
func (s *Store) TransitionAttachmentUploadClaim(ctx context.Context, id, nonce, to, reason string, now time.Time) (bool, error) {
	tag, e := s.db().Exec(ctx, s.q(`UPDATE attachment_uploads SET status=$3, reason=$4, updated_at=$5, claim_nonce=NULL WHERE id=$1 AND status='completing' AND claim_nonce=$2`, `UPDATE attachment_uploads SET status=?,reason=?,updated_at=?,claim_nonce=NULL WHERE id=? AND status='completing' AND claim_nonce=?`), uploadClaimTransitionArgs(s.dialect, id, nonce, to, reason, now)...)
	if e != nil {
		return false, e
	}
	return tag == 1, nil
}
func uploadClaimTransitionArgs(d storeDialect, id, nonce, to, reason string, now time.Time) []any {
	if d == mysqlDialect {
		return []any{to, reason, now, id, nonce}
	}
	return []any{id, nonce, to, reason, now}
}

// TouchAttachmentUploadClaim refreshes the lease of a completing upload the
// caller still holds, so a live completion that streams longer than the lease
// is not reclaimed. It reports whether the claim is still held.
func (s *Store) TouchAttachmentUploadClaim(ctx context.Context, id, nonce string, now time.Time) (bool, error) {
	tag, e := s.db().Exec(ctx, s.q(`UPDATE attachment_uploads SET updated_at=$3 WHERE id=$1 AND status='completing' AND claim_nonce=$2`, `UPDATE attachment_uploads SET updated_at=? WHERE id=? AND status='completing' AND claim_nonce=?`), uploadTouchArgs(s.dialect, id, nonce, now)...)
	if e != nil {
		return false, e
	}
	return tag == 1, nil
}
func uploadTouchArgs(d storeDialect, id, nonce string, now time.Time) []any {
	if d == mysqlDialect {
		return []any{now, id, nonce}
	}
	return []any{id, nonce, now}
}

// MarkAttachmentUploadStagingDeleted releases an upload's staging charge. The
// caller must have confirmed the staging object is absent and that no presigned
// PUT for it can still succeed.
func (s *Store) MarkAttachmentUploadStagingDeleted(ctx context.Context, id string, now time.Time) error {
	args := []any{id, now}
	if s.dialect == mysqlDialect {
		args = []any{now, id}
	}
	_, e := s.db().Exec(ctx, s.q(`UPDATE attachment_uploads SET staging_deleted_at=$2 WHERE id=$1 AND staging_deleted_at IS NULL`, `UPDATE attachment_uploads SET staging_deleted_at=? WHERE id=? AND staging_deleted_at IS NULL`), args...)
	return e
}

// ListUnreleasedAttachmentUploads returns up to limit uploads still charged to
// the budget whose creation is before createdBefore, oldest first. The sweep
// reconciles them against the object store.
func (s *Store) ListUnreleasedAttachmentUploads(ctx context.Context, createdBefore time.Time, limit int) ([]AttachmentUpload, error) {
	rows, err := s.db().Query(ctx, s.q(`SELECT `+uploadColumns+` FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_at < $1 ORDER BY created_at, id LIMIT $2`, `SELECT `+uploadColumns+` FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_at < ? ORDER BY created_at,id LIMIT ?`), createdBefore, limit)
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
// added to committed attachments and to the bytes still charged to the budget,
// stay within storageCap, the charged bytes stay within stagingCap, and the
// bytes charged to its creator stay within clientCap. Charged means the
// staging object may still exist: a row stops counting only after
// MarkAttachmentUploadStagingDeleted, not when it is completed or refused. The
// check and the insert run under one advisory lock, so concurrent begins cannot
// both pass it, and pending reservations count against the storage fuse so
// transient staging spend is bounded by what could ever commit.
func (s *Store) ReserveAttachmentUpload(ctx context.Context, u AttachmentUpload, stagingCap, clientCap, storageCap int64) error {
	return s.retryTx(ctx, func() error {
		return s.reserveAttachmentUploadOnce(ctx, u, stagingCap, clientCap, storageCap)
	})
}

// reserveAttachmentUploadOnce is one attempt of ReserveAttachmentUpload's transaction.
func (s *Store) reserveAttachmentUploadOnce(ctx context.Context, u AttachmentUpload, stagingCap, clientCap, storageCap int64) error {
	if !attachmentUploadIDPattern.MatchString(u.ID) {
		return errors.New("invalid attachment upload id")
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err := lockAttachmentStagingBudget(ctx, s, tx); err != nil {
		return err
	}
	var committed, used, usedByClient int64
	if err := tx.QueryRow(ctx, attachmentHotBytesSQL).Scan(&committed); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM attachment_uploads WHERE staging_deleted_at IS NULL`).Scan(&used); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, s.q(`SELECT COALESCE(SUM(size_bytes), 0) FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_by=$1`, `SELECT COALESCE(SUM(size_bytes), 0) FROM attachment_uploads WHERE staging_deleted_at IS NULL AND created_by=?`), u.CreatedBy).Scan(&usedByClient); err != nil {
		return err
	}
	if committed+used+u.SizeBytes > storageCap {
		return ErrAttachmentStorageCap
	}
	if used+u.SizeBytes > stagingCap || usedByClient+u.SizeBytes > clientCap {
		return ErrAttachmentStagingCap
	}
	if _, err := tx.Exec(ctx, s.q(`INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending','',$10,$11,$10)`, `INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'pending','',?,?,?)`), attachmentUploadInsertArgs(s.dialect, u)...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func attachmentUploadInsertArgs(d storeDialect, u AttachmentUpload) []any {
	args := []any{u.ID, u.StagingKey, u.Name, u.MIME, u.SizeBytes, u.SHA256, u.RefKind, u.RefID, u.CreatedBy, u.CreatedAt, u.ExpiresAt}
	if d == mysqlDialect {
		args = append(args, u.CreatedAt)
	}
	return args
}

func lockAttachmentStagingBudget(ctx context.Context, s *Store, tx transaction) error {
	if s.dialect == mysqlDialect {
		_, err := mysqlTransactionLock(ctx, tx, mysqlLockKey("text", "attachment_staging_budget"), false)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('attachment_staging_budget', 0))`)
	return err
}
