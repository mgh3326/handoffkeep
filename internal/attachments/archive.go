package attachments

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ArchiveMode is HANDOFFKEEP_ATTACH_ARCHIVE_MODE, read once at serve start
// (design/2026-11-16/hk-attachment-archive):
//   - off: the archive endpoint refuses. The default.
//   - record: hk records the archive and keeps the hot copy. This is the
//     canary.
//   - on: hk records the archive and deletes the hot copy after the record
//     commits.
type ArchiveMode string

const (
	ArchiveModeOff    ArchiveMode = "off"
	ArchiveModeRecord ArchiveMode = "record"
	ArchiveModeOn     ArchiveMode = "on"
)

// ErrArchiveOff means the archive endpoint is closed by the mode.
var ErrArchiveOff = errors.New("attachment_archive_off")

// ArchivedError reports an attachment whose hot copy is archived: the bytes
// live at Location, and hk holds no object for it. Restore is not built yet.
type ArchivedError struct {
	SHA256   string
	Location string
}

func (e *ArchivedError) Error() string {
	return "attachment_archived: " + e.SHA256 + " at " + e.Location
}

// Is matches any *ArchivedError, so errors.Is and appErr's table work on a
// fresh target.
func (e *ArchivedError) Is(target error) bool {
	_, ok := target.(*ArchivedError)
	return ok
}

// ArchiveModeFromEnv reads HANDOFFKEEP_ATTACH_ARCHIVE_MODE: unset or "off"
// is off, "record" and "on" are themselves, anything else is a start error.
func ArchiveModeFromEnv() (ArchiveMode, error) {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_ATTACH_ARCHIVE_MODE"))
	switch raw {
	case "", string(ArchiveModeOff):
		return ArchiveModeOff, nil
	case string(ArchiveModeRecord):
		return ArchiveModeRecord, nil
	case string(ArchiveModeOn):
		return ArchiveModeOn, nil
	}
	return "", fmt.Errorf("HANDOFFKEEP_ATTACH_ARCHIVE_MODE=%q: want off, record or on", raw)
}

// minArchiveGrace is the smallest HANDOFFKEEP_ATTACH_ARCHIVE_GRACE a start
// accepts.
const minArchiveGrace = time.Hour

// ArchiveGraceFromEnv reads HANDOFFKEEP_ATTACH_ARCHIVE_GRACE, a Go duration
// (e.g. 24h, 168h): unset or blank is the store's default (7 days), a value
// that does not parse or is below one hour is a start error.
func ArchiveGraceFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_ATTACH_ARCHIVE_GRACE"))
	if raw == "" {
		return store.AttachmentArchiveGrace, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("HANDOFFKEEP_ATTACH_ARCHIVE_GRACE=%q: want a Go duration such as 24h: %w", raw, err)
	}
	if d < minArchiveGrace {
		return 0, fmt.Errorf("HANDOFFKEEP_ATTACH_ARCHIVE_GRACE=%q: want at least %s", raw, minArchiveGrace)
	}
	return d, nil
}

// Candidates pages the blobs whose referring tasks are all closed and whose
// latest close has passed the grace period. It answers in mode off too: the
// dry run that must precede the canary reads it.
func (m *Manager) Candidates(ctx context.Context, limit int, after string) ([]store.AttachmentArchiveCandidate, string, error) {
	if !m.Enabled {
		return nil, "", ErrDisabled
	}
	return m.Store.ListAttachmentArchiveCandidates(ctx, limit, after, m.now())
}

// Archive records the verified archive copy at location, then — in mode on —
// deletes the hot object after the record commits. A failed delete is logged
// as attach.archive.hot_delete_failed and left unmarked, so the next call for
// that sha retries it. The record is still answered either way: the caller
// sees hot_deleted=false and the operator sees the log line.
func (m *Manager) Archive(ctx context.Context, sha, location string, size int64) (store.AttachmentArchive, error) {
	if !m.Enabled {
		return store.AttachmentArchive{}, ErrDisabled
	}
	if m.ArchiveMode == ArchiveModeOff {
		return store.AttachmentArchive{}, ErrArchiveOff
	}
	rec, err := m.Store.RecordAttachmentArchive(ctx, sha, location, size, m.now())
	if err != nil {
		return store.AttachmentArchive{}, err
	}
	if m.ArchiveMode != ArchiveModeOn || rec.HotDeleted {
		return rec, nil
	}
	if err := m.Objects.Delete(ctx, key(sha)); err != nil {
		log.Printf("attach.archive.hot_delete_failed sha=%s err=%q", sha, err.Error())
		return rec, nil
	}
	if err := m.Store.MarkAttachmentHotDeleted(ctx, sha, m.now()); err != nil {
		log.Printf("attach.archive.hot_delete_failed sha=%s err=%q", sha, err.Error())
		return rec, nil
	}
	rec.HotDeleted = true
	return rec, nil
}

// archivedLocked reports the archived state of one stored row. The caller
// already holds the row.
func archived(x store.Attachment) error {
	if x.ArchiveLocation == "" {
		return nil
	}
	return &ArchivedError{SHA256: x.SHA256, Location: x.ArchiveLocation}
}
