package attachments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Direct upload: the client PUTs the bytes straight to the object store with a
// presigned URL, so they never pass through hk. hk checks them on completion.
const (
	// UploadTTL is how long a presigned PUT stays valid.
	UploadTTL = 10 * time.Minute
	// uploadRowTTL is how long a begun upload may wait for its completion.
	uploadRowTTL = time.Hour
	// StagingSweepAge is how old a staging object must be before the sweep
	// deletes it. It exceeds uploadRowTTL, so no upload that could still
	// complete loses its bytes.
	StagingSweepAge = 2 * time.Hour
	stagingPrefix   = "incoming/"
	// uploadLease is how long a completion may hold its claim before another
	// call may reclaim the upload. A claim older than this belongs to a
	// completion that stopped before it finished.
	uploadLease = 5 * time.Minute
	// releaseGrace is slack past a presigned URL's life before a missing staging
	// object is treated as final.
	releaseGrace = time.Minute
	// sweepBatch bounds how many charged uploads one sweep reconciles.
	sweepBatch = 500
)

var (
	ErrUploadNotFound    = errors.New("attachment_upload_not_found")
	ErrUploadExpired     = errors.New("attachment_upload_expired")
	ErrUploadInProgress  = errors.New("attachment_upload_in_progress")
	ErrUploadRefused     = errors.New("attachment_upload_refused")
	ErrUploadNotReceived = errors.New("attachment_upload_not_received")
	// ErrDirectUploadDisabled means HK_ATTACH_DIRECT_UPLOAD is not set.
	ErrDirectUploadDisabled = errors.New("attachment_direct_upload_disabled")
)

var (
	shaPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// UploadTicket is what BeginUpload returns: the upload id to complete with and
// the presigned PUT URL for the staging object.
type UploadTicket struct {
	UploadID         string `json:"upload_id"`
	Method           string `json:"method"`
	URL              string `json:"url"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// BeginUpload checks a declared attachment before any bytes move: name, MIME,
// size, sha256, ref, and capacity. It records a pending upload and returns a
// presigned PUT for its staging object. The bytes are checked at completion.
func (m *Manager) BeginUpload(ctx context.Context, client, name, mime string, size int64, sha, ref string) (UploadTicket, error) {
	if !m.Enabled {
		return UploadTicket{}, ErrDisabled
	}
	if !m.Config.DirectUpload {
		return UploadTicket{}, ErrDirectUploadDisabled
	}
	if err := checkName(name); err != nil {
		return UploadTicket{}, err
	}
	declared := strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	if !m.Config.Allow[declared] {
		return UploadTicket{}, ErrMIME
	}
	if size < 0 || size > m.Config.MaxBytes {
		return UploadTicket{}, ErrTooLarge
	}
	if !shaPattern.MatchString(sha) {
		return UploadTicket{}, errors.New("sha256 must be 64 lowercase hex characters")
	}
	kind, id, err := ParseRef(ref)
	if err != nil {
		return UploadTicket{}, err
	}
	uid, err := newUploadID()
	if err != nil {
		return UploadTicket{}, err
	}
	key := stagingPrefix + uid
	u, err := m.Objects.PresignPut(ctx, key, UploadTTL, size)
	if err != nil {
		return UploadTicket{}, fmt.Errorf("%w: %v", ErrObjectStore, err)
	}
	now := m.now()
	row := store.AttachmentUpload{ID: uid, StagingKey: key, Name: name, MIME: declared, SizeBytes: size, SHA256: sha, RefKind: kind, RefID: id, CreatedBy: client, Status: store.AttachmentUploadPending, CreatedAt: now, ExpiresAt: now.Add(uploadRowTTL), UpdatedAt: now}
	// The reservation checks the staging budget, global and per creator, with
	// the insert; a refusal creates no row. The storage caps are checked first.
	if err := m.Store.CheckAttachmentCapacity(ctx, size, m.Config.StorageCap, m.Config.PutCap); err != nil {
		return UploadTicket{}, err
	}
	if err := m.Store.ReserveAttachmentUpload(ctx, row, m.Config.StagingCap, m.Config.StagingClientCap); err != nil {
		return UploadTicket{}, err
	}
	return UploadTicket{UploadID: uid, Method: "PUT", URL: u, ExpiresInSeconds: int(UploadTTL / time.Second)}, nil
}

// CompleteUpload checks the staged bytes against what BeginUpload declared
// (size, sha256, sniffed type, secret guard), then stores them exactly as Put
// would. A refusal deletes the staging object and is final; a transient
// failure returns the upload to pending so the client can retry. Only the
// principal that began the upload can complete it.
func (m *Manager) CompleteUpload(ctx context.Context, client, uploadID string) (store.Attachment, bool, error) {
	if !m.Enabled {
		return store.Attachment{}, false, ErrDisabled
	}
	if !uploadIDPattern.MatchString(uploadID) {
		return store.Attachment{}, false, ErrUploadNotFound
	}
	row, ok, err := m.Store.GetAttachmentUpload(ctx, uploadID)
	if err != nil {
		return store.Attachment{}, false, err
	}
	if !ok || row.CreatedBy != client {
		return store.Attachment{}, false, ErrUploadNotFound
	}
	now := m.now()
	switch row.Status {
	case store.AttachmentUploadDone:
		x, found, err := m.Store.GetAttachment(ctx, row.SHA256)
		if err != nil {
			return store.Attachment{}, false, err
		}
		if !found {
			return store.Attachment{}, false, ErrUploadNotFound
		}
		return x, false, nil
	case store.AttachmentUploadRefused:
		return store.Attachment{}, false, fmt.Errorf("%w: %s", ErrUploadRefused, row.Reason)
	case store.AttachmentUploadCompleting:
		// A claim older than the lease belongs to a completion that stopped before
		// it moved the row to done. Its staging object is still there (deletion
		// follows done), so the claim goes back to pending and this call redoes
		// the completion; the store path is idempotent, so the redo is safe.
		if now.Before(row.UpdatedAt.Add(uploadLease)) {
			return store.Attachment{}, false, ErrUploadInProgress
		}
		reclaimed, err := m.Store.TransitionAttachmentUpload(ctx, row.ID, store.AttachmentUploadCompleting, store.AttachmentUploadPending, "", now)
		if err != nil {
			return store.Attachment{}, false, err
		}
		if !reclaimed {
			return store.Attachment{}, false, ErrUploadInProgress
		}
		row.Status = store.AttachmentUploadPending
	}
	if !now.Before(row.ExpiresAt) {
		changed, err := m.finishUpload(ctx, row, store.AttachmentUploadPending, store.AttachmentUploadRefused, "upload_expired", now)
		if err != nil {
			return store.Attachment{}, false, err
		}
		if !changed {
			// Another completion claimed the row after this read; its bytes are
			// its own to finish, so this call must not treat them as expired.
			return store.Attachment{}, false, ErrUploadInProgress
		}
		return store.Attachment{}, false, ErrUploadExpired
	}
	claimed, err := m.Store.TransitionAttachmentUpload(ctx, row.ID, store.AttachmentUploadPending, store.AttachmentUploadCompleting, "", now)
	if err != nil {
		return store.Attachment{}, false, err
	}
	if !claimed {
		return store.Attachment{}, false, ErrUploadInProgress
	}
	return m.completeClaimed(ctx, row)
}

// completeClaimed runs with the row in completing. Every exit either moves the
// row to done, to refused (final, staging deleted), or back to pending.
func (m *Manager) completeClaimed(ctx context.Context, row store.AttachmentUpload) (store.Attachment, bool, error) {
	release := func(cause error) (store.Attachment, bool, error) {
		if _, e := m.Store.TransitionAttachmentUpload(ctx, row.ID, store.AttachmentUploadCompleting, store.AttachmentUploadPending, "", m.now()); e != nil {
			return store.Attachment{}, false, e
		}
		return store.Attachment{}, false, fmt.Errorf("%w: %v", ErrUploadNotReceived, cause)
	}
	refuse := func(reason string) (store.Attachment, bool, error) {
		changed, e := m.finishUpload(ctx, row, store.AttachmentUploadCompleting, store.AttachmentUploadRefused, reason, m.now())
		if e != nil {
			return store.Attachment{}, false, e
		}
		if !changed {
			return store.Attachment{}, false, ErrUploadInProgress
		}
		return store.Attachment{}, false, fmt.Errorf("%w: %s", ErrUploadRefused, reason)
	}
	head, err := m.Objects.Head(ctx, row.StagingKey)
	if err != nil {
		return release(err)
	}
	// The server's cap is checked here, not only at begin: the config may have
	// changed since, and the staged object is whatever the client PUT.
	if head.Size > m.Config.MaxBytes {
		return refuse("upload_too_large")
	}
	if head.Size != row.SizeBytes {
		return refuse("upload_size_mismatch")
	}
	// Promotion choice: the content-addressed object is written from the bytes
	// read and verified below, never by copying or re-reading the staging
	// object. A presigned PUT can be reused until it expires, so the staging
	// object may be overwritten after this read; the stored object cannot be,
	// because it is the verified buffer. The staging object is deleted after
	// promotion, and a leftover one is swept.
	r, err := m.Objects.Get(ctx, row.StagingKey)
	if err != nil {
		return release(err)
	}
	// Read at most one byte past the declared size, so a longer object is seen
	// without being read whole.
	body, err := io.ReadAll(io.LimitReader(r, row.SizeBytes+1))
	_ = r.Close()
	if err != nil {
		return release(err)
	}
	if int64(len(body)) != row.SizeBytes {
		return refuse("upload_size_mismatch")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != row.SHA256 {
		return refuse("upload_sha256_mismatch")
	}
	x, created, err := m.putBytes(ctx, row.CreatedBy, row.Name, row.MIME, uploadRefString(row), body)
	if err != nil {
		reason, refusable := uploadRefusalReason(err)
		if !refusable {
			return release(err)
		}
		return refuse(reason)
	}
	if _, err := m.Store.TransitionAttachmentUpload(ctx, row.ID, store.AttachmentUploadCompleting, store.AttachmentUploadDone, "", m.now()); err != nil {
		return x, created, err
	}
	m.deleteStaging(ctx, row.StagingKey)
	return x, created, nil
}

// finishUpload moves a row to a final state and deletes its staging object,
// but only if this call made the transition. When the row was not in from, a
// concurrent completion owns it and the staging object is not this call's to
// delete. The delete is best effort: the state is already final, and the sweep
// removes any object that survives.
func (m *Manager) finishUpload(ctx context.Context, row store.AttachmentUpload, from, to, reason string, now time.Time) (bool, error) {
	changed, err := m.Store.TransitionAttachmentUpload(ctx, row.ID, from, to, reason, now)
	if err != nil || !changed {
		return changed, err
	}
	m.deleteStaging(ctx, row.StagingKey)
	return true, nil
}

func (m *Manager) deleteStaging(ctx context.Context, key string) {
	_ = m.Objects.Delete(ctx, key)
}

// SweepStaging does two things, in order. It deletes staging objects older than
// StagingSweepAge, which removes any left by a failed best-effort delete. Then
// it releases the budget charge of each upload whose staging object is confirmed
// absent and whose presigned URL is past its life: a PUT cannot recreate the
// object after that, so the charge is no longer needed. A charge is never
// released on the upload's state or age alone. It returns how many objects it
// deleted.
func (m *Manager) SweepStaging(ctx context.Context) (int, error) {
	if !m.Enabled {
		return 0, ErrDisabled
	}
	now := m.now()
	objs, err := m.Objects.List(ctx, stagingPrefix)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, o := range objs {
		if o.Modified.After(now.Add(-StagingSweepAge)) {
			continue
		}
		if err := m.Objects.Delete(ctx, o.Key); err != nil {
			return deleted, err
		}
		deleted++
	}
	charged, err := m.Store.ListUnreleasedAttachmentUploads(ctx, now.Add(-(UploadTTL + releaseGrace)), sweepBatch)
	if err != nil {
		return deleted, err
	}
	for _, u := range charged {
		// Any Head failure other than "absent" leaves the charge in place: the
		// object's state is unknown, so the budget keeps counting it.
		if _, err := m.Objects.Head(ctx, u.StagingKey); !errors.Is(err, ErrObjectNotFound) {
			continue
		}
		if err := m.Store.MarkAttachmentUploadStagingDeleted(ctx, u.ID, now); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// uploadRefusalReason names the refusals that are final: the bytes or the
// declaration are wrong. Any other error is a storage fault and is retryable.
// The reason is a fixed code, never the matched text, so a secret pattern is
// not echoed back to the client.
func uploadRefusalReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrTooLarge):
		return "attachment_too_large", true
	case errors.Is(err, ErrMIME):
		return "attachment_mime_rejected", true
	case errors.Is(err, ErrExtension):
		return "attachment_extension_rejected", true
	case errors.Is(err, store.ErrAttachmentStorageCap):
		return "attachment_storage_cap", true
	case errors.Is(err, store.ErrAttachmentPutCap):
		return "attachment_put_cap", true
	case isSecretRefusal(err):
		return "secret_like_content", true
	}
	return "", false
}

// uploadRefString is the ref in the form ParseRef reads, so putBytes records
// the same ref the client declared.
func uploadRefString(row store.AttachmentUpload) string {
	if row.RefKind == "" || row.RefKind == "none" {
		return ""
	}
	return row.RefKind + ":" + row.RefID
}

func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isSecretRefusal(err error) bool {
	_, ok := guard.SecretPattern(err)
	return ok
}
