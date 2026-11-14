package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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
	// call may reclaim the upload. A claim whose heartbeat has been silent
	// this long belongs to a completion that stopped before it finished.
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

// uploadHeartbeat is how often a live completion refreshes its claim lease.
// It is a var so tests can shorten it.
var uploadHeartbeat = 30 * time.Second

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
	if !store.ValidActor(client) {
		return UploadTicket{}, errors.New("invalid attachment created_by")
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
	// A text upload above the buffered-completion fuse can never complete:
	// its secret scan needs the whole body in memory. Refuse it here, before
	// any bytes move; binary types complete by streaming (hk 4466).
	if size > BufferedCompletionMaxBytes && isText(declared) {
		return UploadTicket{}, fmt.Errorf("%w: upload_completion_buffer_limit_exceeded", ErrUploadRefused)
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
	// The reservation checks the staging budget, global and per creator, and
	// the storage fuse including every still-charged pending upload, with the
	// insert; a refusal creates no row. The capacity preflight runs first.
	if err := m.Store.CheckAttachmentCapacity(ctx, size, m.Config.StorageCap, m.Config.PutCap, ""); err != nil {
		return UploadTicket{}, err
	}
	if err := m.Store.ReserveAttachmentUpload(ctx, row, m.Config.StagingCap, m.Config.StagingClientCap, m.Config.StorageCap); err != nil {
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
		// A claim whose heartbeat has been silent for the lease belongs to a
		// completion that stopped before it moved the row to done. Its staging
		// object is still there (deletion follows done), so the claim goes back
		// to pending and this call redoes the completion; the store path is
		// idempotent, so the redo is safe.
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
	nonce, err := newClaimNonce()
	if err != nil {
		return store.Attachment{}, false, err
	}
	claimed, err := m.Store.ClaimAttachmentUpload(ctx, row.ID, nonce, now)
	if err != nil {
		return store.Attachment{}, false, err
	}
	if !claimed {
		return store.Attachment{}, false, ErrUploadInProgress
	}
	row.Status = store.AttachmentUploadCompleting
	row.ClaimNonce = nonce
	row.UpdatedAt = now
	return m.completeClaimed(ctx, row)
}

// completeClaimed runs with the row in completing under this call's claim
// nonce. Every exit either moves the row to done, to refused (final, staging
// deleted), or back to pending — and only while the claim is still held, so a
// completion whose lease was reclaimed cannot move the row another completion
// owns.
func (m *Manager) completeClaimed(ctx context.Context, row store.AttachmentUpload) (store.Attachment, bool, error) {
	stopBeat := m.heartbeatUpload(ctx, row)
	defer stopBeat()
	release := func(cause error) (store.Attachment, bool, error) {
		changed, e := m.Store.TransitionAttachmentUploadClaim(ctx, row.ID, row.ClaimNonce, store.AttachmentUploadPending, "", m.now())
		if e != nil {
			return store.Attachment{}, false, e
		}
		if !changed {
			return store.Attachment{}, false, ErrUploadInProgress
		}
		return store.Attachment{}, false, fmt.Errorf("%w: %v", ErrUploadNotReceived, cause)
	}
	refuse := func(reason string) (store.Attachment, bool, error) {
		changed, e := m.Store.TransitionAttachmentUploadClaim(ctx, row.ID, row.ClaimNonce, store.AttachmentUploadRefused, reason, m.now())
		if e != nil {
			return store.Attachment{}, false, e
		}
		if !changed {
			return store.Attachment{}, false, ErrUploadInProgress
		}
		m.deleteStaging(ctx, row.StagingKey)
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
	if row.SizeBytes <= BufferedCompletionMaxBytes {
		return m.completeBuffered(ctx, row, release, refuse)
	}
	return m.completeStreamed(ctx, row, release, refuse)
}

// completeBuffered verifies an upload small enough to hold in memory, then
// stores it through the buffered path.
func (m *Manager) completeBuffered(ctx context.Context, row store.AttachmentUpload, release func(error) (store.Attachment, bool, error), refuse func(string) (store.Attachment, bool, error)) (store.Attachment, bool, error) {
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
	mime, err := validate(m.Config, row.Name, row.MIME, body)
	if err != nil {
		reason, refusable := uploadRefusalReason(err)
		if !refusable {
			return release(err)
		}
		return refuse(reason)
	}
	x, created, err := m.putVerified(ctx, row.CreatedBy, row.Name, mime, uploadRefString(row), row.SHA256, row.SizeBytes, row.ID, false,
		m.finalizeCommit(ctx, row),
		func(ctx context.Context) error {
			return m.Objects.Put(ctx, key(row.SHA256), bytes.NewReader(body), row.SizeBytes, mime)
		})
	if err != nil {
		if errors.Is(err, store.ErrUploadClaimLost) {
			return m.uploadFinalizeFallback(ctx, row)
		}
		reason, refusable := uploadRefusalReason(err)
		if !refusable {
			return release(err)
		}
		return refuse(reason)
	}
	m.deleteStaging(ctx, row.StagingKey)
	return x, created, nil
}

// verifyHeadBytes is how much of a streamed upload is kept for the sniff and
// NUL checks; both read at most this much of a body.
const verifyHeadBytes = 8 << 10

// streamVerifier observes what an ObjectStore Put consumes: the byte count,
// the sha256 and the first verifyHeadBytes for content sniffing.
type streamVerifier struct {
	r    io.Reader
	sum  hash.Hash
	n    int64
	head []byte
}

func (v *streamVerifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.sum.Write(p[:n])
		v.n += int64(n)
		if rem := verifyHeadBytes - len(v.head); rem > 0 {
			if n < rem {
				rem = n
			}
			v.head = append(v.head, p[:rem]...)
		}
	}
	return n, err
}

// overflow reports whether the stream held a byte past what the Put consumed
// (the LimitReader caps it at one).
func (v *streamVerifier) overflow() bool {
	var b [1]byte
	n, _ := v.r.Read(b[:])
	return n > 0
}

// completeStreamed verifies an upload too large to hold in memory. The staged
// object is copied to a private holding object while size and sha256 are
// checked in the same pass; only the verified holding object is promoted to
// the content key, re-verified as it streams, so the stored object is always
// the checked bytes and a staging overwrite after the read changes nothing.
// Text types stay on the buffered path: their secret scan needs the body.
func (m *Manager) completeStreamed(ctx context.Context, row store.AttachmentUpload, release func(error) (store.Attachment, bool, error), refuse func(string) (store.Attachment, bool, error)) (store.Attachment, bool, error) {
	if isText(row.MIME) {
		return refuse("upload_completion_buffer_limit_exceeded")
	}
	// The holding key is per claim: a completion whose lease was reclaimed can
	// still be streaming, and two claims must not overwrite each other's
	// holding object mid-copy.
	holdingKey := stagingPrefix + "done-" + row.ID + "-" + row.ClaimNonce
	// Whatever happens next, the holding object is private and disposable.
	defer func() { _ = m.Objects.Delete(ctx, holdingKey) }()
	r, err := m.Objects.Get(ctx, row.StagingKey)
	if err != nil {
		return release(err)
	}
	// The Put consumes at most the declared size; the byte past it, when the
	// staging object is longer, is seen by overflow without being stored.
	v := &streamVerifier{r: io.LimitReader(r, row.SizeBytes+1), sum: sha256.New(), head: make([]byte, 0, verifyHeadBytes)}
	putErr := m.Objects.Put(ctx, holdingKey, v, row.SizeBytes, row.MIME)
	// overflow reads one byte past what the Put consumed, so it must run
	// before the staging body is closed: a real object store invalidates the
	// body on Close.
	grown := v.overflow()
	_ = r.Close()
	if putErr != nil {
		return release(putErr)
	}
	if v.n != row.SizeBytes || grown {
		return refuse("upload_size_mismatch")
	}
	if hex.EncodeToString(v.sum.Sum(nil)) != row.SHA256 {
		return refuse("upload_sha256_mismatch")
	}
	mime, err := validateStreamed(m.Config, row.Name, row.MIME, v.head)
	if err != nil {
		reason, refusable := uploadRefusalReason(err)
		if !refusable {
			return release(err)
		}
		return refuse(reason)
	}
	x, created, err := m.putVerified(ctx, row.CreatedBy, row.Name, mime, uploadRefString(row), row.SHA256, row.SizeBytes, row.ID, true,
		m.finalizeCommit(ctx, row),
		func(ctx context.Context) error {
			tr, err := m.Objects.Get(ctx, holdingKey)
			if err != nil {
				return err
			}
			defer tr.Close()
			pv := &streamVerifier{r: tr, sum: sha256.New()}
			// Promotion output stays on a private per-claim key until verification
			// passes: a concurrent putVerified for the same sha can Stat the shared
			// content key and register it, so publishing unverified bytes there and
			// deleting them on a failed check would leave metadata pointing at a
			// missing object. Only a server-side copy of the verified private
			// object publishes, and a failed check deletes just the private key.
			promoKey := stagingPrefix + "promo-" + row.ID + "-" + row.ClaimNonce
			defer func() { _ = m.Objects.Delete(ctx, promoKey) }()
			if err := m.Objects.Put(ctx, promoKey, pv, row.SizeBytes, mime); err != nil {
				return err
			}
			if pv.n != row.SizeBytes || pv.overflow() || hex.EncodeToString(pv.sum.Sum(nil)) != row.SHA256 {
				return errors.New("promoted object failed verification")
			}
			published := key(row.SHA256)
			// The published object is what readers adopt, so it is re-hashed
			// after the copy — a backend fault between promo and content key
			// must not reach the dedup path. A completed verification that
			// still mismatches proves the key does not hold this sha's bytes,
			// so it is deleted on a detached context (the request ctx may
			// already be cancelled) — it cannot be valid committed content.
			// A copy or read error leaves the object's state unknown and a
			// concurrent commit may reference it, so nothing is deleted then:
			// alwaysWrite makes the next attempt re-copy and re-verify,
			// which repairs whatever is there.
			verified, provenBad := false, false
			defer func() {
				if !verified && provenBad {
					dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()
					_ = m.Objects.Delete(dctx, published)
				}
			}()
			for attempt := 0; attempt < 2 && !verified; attempt++ {
				// Each attempt starts unknown: only a verification completed
				// against this attempt's copy may prove the key bad. A failed
				// Copy can leave a concurrent publish intact, so a stale
				// provenBad must not survive into it.
				provenBad = false
				if err := m.Objects.Copy(ctx, promoKey, published); err != nil {
					return err
				}
				dtr, err := m.Objects.Get(ctx, published)
				if err != nil {
					return err
				}
				dv := &streamVerifier{r: io.LimitReader(dtr, row.SizeBytes+1), sum: sha256.New()}
				_, err = io.Copy(io.Discard, dv)
				dtr.Close()
				if err != nil {
					return err
				}
				if dv.n == row.SizeBytes && hex.EncodeToString(dv.sum.Sum(nil)) == row.SHA256 {
					verified = true
				} else {
					provenBad = true
				}
			}
			if !verified {
				return errors.New("published object failed verification")
			}
			return nil
		})
	if err != nil {
		if errors.Is(err, store.ErrUploadClaimLost) {
			return m.uploadFinalizeFallback(ctx, row)
		}
		reason, refusable := uploadRefusalReason(err)
		if !refusable {
			return release(err)
		}
		return refuse(reason)
	}
	m.deleteStaging(ctx, row.StagingKey)
	return x, created, nil
}

// finalizeCommit is the metadata commit for a completing upload:
// FinalizeAttachmentUpload validates the claim, inserts or dedups the
// attachment, charges the fuses and marks the row done in one transaction, so
// the reservation is consumed exactly when the bytes commit.
func (m *Manager) finalizeCommit(ctx context.Context, row store.AttachmentUpload) func(store.Attachment) (store.Attachment, bool, error) {
	return func(x store.Attachment) (store.Attachment, bool, error) {
		return m.Store.FinalizeAttachmentUpload(ctx, row.ID, row.ClaimNonce, x, m.Config.StorageCap, m.Config.PutCap)
	}
}

// uploadFinalizeFallback answers a completion whose claim-scoped finalize did
// not apply because its lease was reclaimed while it ran. The row is re-read
// so the caller learns the truth: done means another completion of the same
// verified bytes finished, refused means the upload is final, anything else
// means a completion still owns it.
func (m *Manager) uploadFinalizeFallback(ctx context.Context, row store.AttachmentUpload) (store.Attachment, bool, error) {
	cur, ok, err := m.Store.GetAttachmentUpload(ctx, row.ID)
	if err != nil {
		return store.Attachment{}, false, err
	}
	if ok && cur.Status == store.AttachmentUploadDone {
		x, found, err := m.Store.GetAttachment(ctx, row.SHA256)
		if err != nil {
			return store.Attachment{}, false, err
		}
		if found {
			return x, false, nil
		}
	}
	if ok && cur.Status == store.AttachmentUploadRefused {
		return store.Attachment{}, false, fmt.Errorf("%w: %s", ErrUploadRefused, cur.Reason)
	}
	return store.Attachment{}, false, ErrUploadInProgress
}

// heartbeatUpload refreshes the claim lease while a completion runs, so a
// streamed upload that takes longer than uploadLease is not reclaimed. The
// touch is claim-scoped: once the lease is truly lost the beats stop matching
// and the row can be reclaimed. The returned func stops the heartbeat.
func (m *Manager) heartbeatUpload(ctx context.Context, row store.AttachmentUpload) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	every := uploadHeartbeat // read once here, not inside the goroutine
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = m.Store.TouchAttachmentUploadClaim(ctx, row.ID, row.ClaimNonce, m.now())
			}
		}
	}()
	// Stop waits for the goroutine, so no beat runs after the completion ends.
	return func() { close(done); <-exited }
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

func newUploadID() (string, error) { return randomHex() }

// newClaimNonce is the per-claim token a completion presents on every
// transition out of completing.
func newClaimNonce() (string, error) { return randomHex() }

func randomHex() (string, error) {
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
