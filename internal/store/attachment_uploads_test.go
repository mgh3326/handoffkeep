package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Direct-upload rows: create, read, and a transition that only one caller can
// win. Runs in its own schema because it counts rows.
func TestAttachmentUploadRowsTransitionOnce(t *testing.T) {
	s := schemaStore(t, "attachment_uploads")
	now := time.Now().UTC().Truncate(time.Microsecond)
	const id = "0123456789abcdef0123456789abcdef"
	row := AttachmentUpload{ID: id, StagingKey: "incoming/" + id, Name: "a.txt", MIME: "text/plain", SizeBytes: 3, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RefKind: "task", RefID: "12", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateAttachmentUpload(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAttachmentUpload(t.Context(), id)
	if err != nil || !ok || got.Status != AttachmentUploadPending || got.RefKind != "task" || got.RefID != "12" || got.SizeBytes != 3 {
		t.Fatalf("get ok=%v err=%v row=%+v", ok, err, got)
	}
	if _, ok, err := s.GetAttachmentUpload(t.Context(), "ffffffffffffffffffffffffffffffff"); err != nil || ok {
		t.Fatalf("unknown id ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.GetAttachmentUpload(t.Context(), "not-an-id"); ok {
		t.Fatal("malformed id was read")
	}
	claimed, err := s.TransitionAttachmentUpload(t.Context(), id, AttachmentUploadPending, AttachmentUploadCompleting, "", now)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	again, err := s.TransitionAttachmentUpload(t.Context(), id, AttachmentUploadPending, AttachmentUploadCompleting, "", now)
	if err != nil || again {
		t.Fatalf("second claim won=%v err=%v", again, err)
	}
	if err := s.CreateAttachmentUpload(t.Context(), AttachmentUpload{ID: "bad"}); err == nil {
		t.Fatal("malformed upload id stored")
	}
}

// A completion's claim nonce scopes every transition out of completing: only
// the nonce holder can move or heartbeat the row, the nonce is consumed by the
// final transition, and a reclaimed claim's stale nonce matches nothing.
func TestAttachmentUploadClaimNonceScopesTransitions(t *testing.T) {
	s := schemaStore(t, "attachment_upload_claim")
	now := time.Now().UTC().Truncate(time.Microsecond)
	const id = "0123456789abcdef0123456789abcdef"
	row := AttachmentUpload{ID: id, StagingKey: "incoming/" + id, Name: "a.txt", MIME: "text/plain", SizeBytes: 3, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RefKind: "none", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateAttachmentUpload(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimAttachmentUpload(t.Context(), id, "nonce-a", now); err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	got, _, _ := s.GetAttachmentUpload(t.Context(), id)
	if got.Status != AttachmentUploadCompleting || got.ClaimNonce != "nonce-a" {
		t.Fatalf("claimed row=%+v", got)
	}
	// A second claim cannot win while the first holds the row.
	if ok, err := s.ClaimAttachmentUpload(t.Context(), id, "nonce-b", now); err != nil || ok {
		t.Fatalf("second claim won=%v err=%v", ok, err)
	}
	// Neither a transition nor a heartbeat with the wrong nonce applies.
	if ok, err := s.TransitionAttachmentUploadClaim(t.Context(), id, "nonce-b", AttachmentUploadDone, "", now); err != nil || ok {
		t.Fatalf("wrong-nonce transition=%v err=%v", ok, err)
	}
	if ok, err := s.TouchAttachmentUploadClaim(t.Context(), id, "nonce-b", now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("wrong-nonce touch=%v err=%v", ok, err)
	}
	// The holder's heartbeat refreshes the lease timestamp.
	if ok, err := s.TouchAttachmentUploadClaim(t.Context(), id, "nonce-a", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("holder touch=%v err=%v", ok, err)
	}
	got, _, _ = s.GetAttachmentUpload(t.Context(), id)
	if !got.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("heartbeat updated_at=%v", got.UpdatedAt)
	}
	// The holder's transition to done consumes the claim.
	if ok, err := s.TransitionAttachmentUploadClaim(t.Context(), id, "nonce-a", AttachmentUploadDone, "", now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("done=%v err=%v", ok, err)
	}
	got, _, _ = s.GetAttachmentUpload(t.Context(), id)
	if got.Status != AttachmentUploadDone || got.ClaimNonce != "" {
		t.Fatalf("done row=%+v", got)
	}
	if ok, err := s.TransitionAttachmentUploadClaim(t.Context(), id, "nonce-a", AttachmentUploadRefused, "x", now); err != nil || ok {
		t.Fatalf("post-done transition=%v err=%v", ok, err)
	}
}

// The staging budget is checked and the row inserted under one lock. A finished
// upload stays charged until its staging object is marked deleted, and the
// per-client cap applies on top of the global one.
func TestReserveAttachmentUploadEnforcesTheStagingBudget(t *testing.T) {
	s := schemaStore(t, "attachment_budget")
	now := time.Now().UTC().Truncate(time.Microsecond)
	mk := func(id, client string, size int64) AttachmentUpload {
		return AttachmentUpload{ID: id, StagingKey: "incoming/" + id, Name: "a.txt", MIME: "text/plain", SizeBytes: size, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RefKind: "none", CreatedBy: client, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const c = "cccccccccccccccccccccccccccccccc"
	// storageCap is far above every staging cap here so only the staging
	// budget binds; the storage-fuse case has its own test below.
	const storageCap = int64(1) << 40
	if err := s.ReserveAttachmentUpload(t.Context(), mk(a, "node", 60), 100, 100, storageCap); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(b, "other", 60), 100, 100, storageCap); !errors.Is(err, ErrAttachmentStagingCap) {
		t.Fatalf("over-budget reserve err=%v", err)
	}
	if _, ok, _ := s.GetAttachmentUpload(t.Context(), b); ok {
		t.Fatal("a refused reservation left a row")
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(c, "node", 60), 1000, 100, storageCap); !errors.Is(err, ErrAttachmentStagingCap) {
		t.Fatalf("per-client reserve err=%v", err)
	}
	if ok, err := s.TransitionAttachmentUpload(t.Context(), a, AttachmentUploadPending, AttachmentUploadDone, "", now); err != nil || !ok {
		t.Fatalf("finish first: ok=%v err=%v", ok, err)
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(b, "other", 60), 100, 100, storageCap); !errors.Is(err, ErrAttachmentStagingCap) {
		t.Fatalf("done upload released its charge before its staging object was confirmed gone: %v", err)
	}
	if err := s.MarkAttachmentUploadStagingDeleted(t.Context(), a, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(b, "other", 60), 100, 100, storageCap); err != nil {
		t.Fatalf("reserve after the staging object was confirmed gone: %v", err)
	}
	unreleased, err := s.ListUnreleasedAttachmentUploads(t.Context(), now.Add(time.Hour), 10)
	if err != nil || len(unreleased) != 1 || unreleased[0].ID != b {
		t.Fatalf("unreleased=%v err=%v", unreleased, err)
	}
}

// Pending reservations count against the storage fuse at begin: committed
// bytes plus still-charged upload bytes plus the new declaration may not
// exceed it, even when every staging cap would allow the upload.
func TestReserveAttachmentUploadCountsPendingAgainstStorageCap(t *testing.T) {
	s := schemaStore(t, "attachment_pending_storage")
	now := time.Now().UTC().Truncate(time.Microsecond)
	mk := func(id string, size int64) AttachmentUpload {
		return AttachmentUpload{ID: id, StagingKey: "incoming/" + id, Name: "a.txt", MIME: "text/plain", SizeBytes: size, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RefKind: "none", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const c = "cccccccccccccccccccccccccccccccc"
	// stagingCap and clientCap are far above storageCap, so only the storage
	// fuse can refuse.
	if err := s.ReserveAttachmentUpload(t.Context(), mk(a, 40), 1<<40, 1<<40, 100); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(b, 50), 1<<40, 1<<40, 100); err != nil {
		t.Fatalf("second reserve inside the fuse: %v", err)
	}
	if err := s.ReserveAttachmentUpload(t.Context(), mk(c, 20), 1<<40, 1<<40, 100); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("pending over the storage fuse err=%v", err)
	}
	if _, ok, _ := s.GetAttachmentUpload(t.Context(), c); ok {
		t.Fatal("a storage-cap refusal left a row")
	}
}

// Buffered writes count every upload reservation that can still commit —
// pending and completing — against the storage fuse, under the same advisory
// lock ReserveAttachmentUpload takes: without it a buffered Put could spend
// bytes an upload already reserved and doom the completion. A completing
// upload's own commit is the exception: FinalizeAttachmentUpload excludes its
// own row so the reservation is consumed exactly once, atomically with the
// metadata and the done transition.
func TestPutAttachmentCountsPendingUploadReservations(t *testing.T) {
	s := schemaStore(t, "attachment_pending_put")
	now := time.Now().UTC().Truncate(time.Microsecond)
	sha := func(c byte) string { return strings.Repeat(string(c), 64) }
	att := func(c byte, size int64) Attachment {
		return Attachment{SHA256: sha(c), SizeBytes: size, MIME: "text/plain", OriginalName: "a.txt", CreatedBy: "node", RefKind: "none"}
	}
	upload := AttachmentUpload{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StagingKey: "incoming/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "a.txt", MIME: "text/plain", SizeBytes: 90, SHA256: sha('f'), RefKind: "none", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	// The reservation itself passes because its own storageCap argument is
	// generous; the fuse under test is the one PutAttachment/CheckAttachmentCapacity
	// apply to later writes.
	if err := s.ReserveAttachmentUpload(t.Context(), upload, 1<<40, 1<<40, 1<<40); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	const fuse = int64(100)
	if err := s.CheckAttachmentCapacity(t.Context(), 20, fuse, 1<<40, ""); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("preflight over pending+committed err=%v", err)
	}
	if _, _, err := s.PutAttachment(t.Context(), att('a', 20), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("put over pending+committed err=%v", err)
	}
	// 90 pending + 10 committed fits exactly; the 11th byte does not.
	if _, created, err := s.PutAttachment(t.Context(), att('a', 10), fuse, 1<<40); err != nil || !created {
		t.Fatalf("put inside the fuse created=%v err=%v", created, err)
	}
	if _, _, err := s.PutAttachment(t.Context(), att('b', 1), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("put past the fuse err=%v", err)
	}
	// Once claimed the upload is completing, not pending — but its bytes can
	// still commit, so other writers keep paying for the reservation.
	if ok, err := s.ClaimAttachmentUpload(t.Context(), upload.ID, "nonce", now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, _, err := s.PutAttachment(t.Context(), att('c', 11), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("unrelated put spent a completing upload's reservation: %v", err)
	}
	// The completing upload finalizes its own reservation: 10 committed + its
	// own 90 fits exactly, the done transition consumes the charge, and a
	// second finalize under the same nonce is fenced out.
	if _, created, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "nonce", att('f', 90), fuse, 1<<40); err != nil || !created {
		t.Fatalf("finalize inside the fuse created=%v err=%v", created, err)
	}
	if _, _, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "nonce", att('f', 90), fuse, 1<<40); !errors.Is(err, ErrUploadClaimLost) {
		t.Fatalf("second finalize err=%v", err)
	}
	if _, _, err := s.PutAttachment(t.Context(), att('d', 1), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("committed bytes escaped the fuse after finalize: %v", err)
	}
}

// FinalizeAttachmentUpload refuses a claim that does not own the row: a wrong
// nonce, a pending row, or bytes that do not match the reservation.
func TestFinalizeAttachmentUploadFencesClaims(t *testing.T) {
	s := schemaStore(t, "attachment_finalize_fence")
	now := time.Now().UTC().Truncate(time.Microsecond)
	sha := strings.Repeat("e", 64)
	upload := AttachmentUpload{ID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", StagingKey: "incoming/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Name: "a.txt", MIME: "text/plain", SizeBytes: 5, SHA256: sha, RefKind: "none", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	att := Attachment{SHA256: sha, SizeBytes: 5, MIME: "text/plain", OriginalName: "a.txt", CreatedBy: "node", RefKind: "none"}
	if err := s.ReserveAttachmentUpload(t.Context(), upload, 1<<40, 1<<40, 1<<40); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, _, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "nonce", att, 1<<40, 1<<40); !errors.Is(err, ErrUploadClaimLost) {
		t.Fatalf("pending finalize err=%v", err)
	}
	if ok, err := s.ClaimAttachmentUpload(t.Context(), upload.ID, "nonce", now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, _, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "other", att, 1<<40, 1<<40); !errors.Is(err, ErrUploadClaimLost) {
		t.Fatalf("wrong-nonce finalize err=%v", err)
	}
	wrong := att
	wrong.SizeBytes = 6
	if _, _, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "nonce", wrong, 1<<40, 1<<40); !errors.Is(err, ErrUploadClaimLost) {
		t.Fatalf("size-mismatch finalize err=%v", err)
	}
	if _, created, err := s.FinalizeAttachmentUpload(t.Context(), upload.ID, "nonce", att, 1<<40, 1<<40); err != nil || !created {
		t.Fatalf("finalize created=%v err=%v", created, err)
	}
	if u, ok, err := s.GetAttachmentUpload(t.Context(), upload.ID); err != nil || !ok || u.Status != AttachmentUploadDone {
		t.Fatalf("upload after finalize=%+v ok=%v err=%v", u, ok, err)
	}
}
