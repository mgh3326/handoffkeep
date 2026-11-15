// Package attachmentarchive copies hk attachments into the Drive archive and
// records each copy with hk (design 2026-11-16/hk-attachment-archive, hk 4850).
//
// One run is a dry run unless Options.Apply is set. With Apply, a candidate's
// bytes are written to a temp file beside their final path, fsynced and
// renamed; the copy is read back and checked against the candidate's size and
// sha; every referring task's manifest.json is updated atomically; and only
// then is the archive recorded with hk. A rerun converges: a stored copy with
// the right sha is not written again, and a recorded copy answers its record
// idempotently.
package attachmentarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/client"
)

const (
	// MarkerFile is the operator's one-time marker in the archive root. A
	// root without it is refused: a Drive mount that is not mounted would
	// otherwise take the copies onto the local disk.
	MarkerFile = ".hk-archive-root"
	// ArchiveDir is the archive path under the root, relative to it.
	ArchiveDir = "fleet-artifacts/archive"
	// PageSize is the number of candidates one candidates request asks for.
	PageSize = 100
	// maxNameBytes bounds the safe original name: a sha256 (64 bytes) plus
	// the joining "-" and this cap stay under the 255-byte limit on one file
	// name.
	maxNameBytes = 180
)

// Failure reasons in Summary.Failed. Each is also the suffix of the log
// event the failure line carries.
const (
	ReasonCandidateRefused = "candidate_refused"
	ReasonDownloadFailed   = "download_failed"
	ReasonWriteFailed      = "write_failed"
	ReasonVerifyFailed     = "verify_failed"
	ReasonFileMismatch     = "file_mismatch"
	ReasonManifestFailed   = "manifest_failed"
	ReasonRecordFailed     = "record_failed"
)

// API is the hk calls the archiver makes. *client.Client implements it.
type API interface {
	ArchiveCandidates(ctx context.Context, limit int, after string) (client.ArchiveCandidatesPage, error)
	GetAttachment(ctx context.Context, sha string) (client.Attachment, io.ReadCloser, error)
	RecordAttachmentArchive(ctx context.Context, sha, location string, size int64) (client.ArchiveRecord, error)
}

// Options is one run's inputs.
type Options struct {
	// Root is the Drive mount that holds MarkerFile.
	Root string
	// Apply copies, writes manifests and records. False is a dry run.
	Apply bool
	// Limit is the most candidates to process; 0 means every candidate.
	Limit int
}

// Summary is one run's JSON answer.
type Summary struct {
	DryRun         bool      `json:"dry_run"`
	Candidates     int       `json:"candidates"`
	Copied         int       `json:"copied"`
	AlreadyPresent int       `json:"already_present"`
	Recorded       int       `json:"recorded"`
	Failed         []Failure `json:"failed"`
	// Planned lists the files a dry run would write.
	Planned []Planned `json:"planned,omitempty"`
}

// Failure is one candidate a run did not finish, with the reason it stopped.
// A candidate listed here is never recorded.
type Failure struct {
	SHA256 string `json:"sha256"`
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Planned is one file a dry run would write.
type Planned struct {
	SHA256 string `json:"sha256"`
	TaskID int64  `json:"task_id"`
	Path   string `json:"path"`
}

// The two outcomes stage reports for one candidate.
const (
	actionCopy    = "copy"
	actionPresent = "present"
)

// Archive runs one pass over the archive candidates. checkRoot runs first, so
// an unmountable or unmarked root writes nothing. Candidate-reading failures
// end the run; a failure on one candidate is reported in Summary.Failed and
// the pass continues.
func Archive(ctx context.Context, api API, opts Options) (Summary, error) {
	r := &runner{api: api, opts: opts, now: func() time.Time { return time.Now().UTC() }}
	return r.run(ctx)
}

type runner struct {
	api  API
	opts Options
	now  func() time.Time
}

func (r *runner) run(ctx context.Context) (Summary, error) {
	sum := Summary{DryRun: !r.opts.Apply, Failed: []Failure{}}
	if err := checkRoot(r.opts.Root); err != nil {
		return sum, err
	}
	after := ""
	for {
		page, err := r.api.ArchiveCandidates(ctx, PageSize, after)
		if err != nil {
			return sum, fmt.Errorf("archive_candidates_failed: %w", err)
		}
		for _, c := range page.Candidates {
			if r.opts.Limit > 0 && sum.Candidates >= r.opts.Limit {
				return sum, nil
			}
			sum.Candidates++
			r.one(ctx, c, &sum)
		}
		// A Next that does not advance would page forever; the server's
		// sha ordering makes a repeat impossible, so stop rather than loop.
		if page.Next == "" || page.Next == after {
			return sum, nil
		}
		after = page.Next
	}
}

// one archives one candidate: copy if needed, update every referring task's
// manifest, then record with hk. Any step that fails appends a Failure and
// skips the record.
func (r *runner) one(ctx context.Context, c client.ArchiveCandidate, sum *Summary) {
	if !validSHA(c.SHA256) || c.TaskID <= 0 {
		r.fail(sum, c, &stageError{ReasonCandidateRefused, "attachment_archive.candidate_failed", fmt.Errorf("sha %q or task %d is not usable", c.SHA256, c.TaskID)})
		return
	}
	rel := storedPath(c)
	abs := filepath.Join(r.opts.Root, filepath.FromSlash(rel))
	action, err := r.stage(ctx, c, abs)
	if err != nil {
		var se *stageError
		if !errors.As(err, &se) {
			se = &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
		}
		r.fail(sum, c, se)
		return
	}
	if !r.opts.Apply {
		if action == actionCopy {
			sum.Planned = append(sum.Planned, Planned{SHA256: c.SHA256, TaskID: c.TaskID, Path: rel})
			log.Printf("attachment_archive.planned sha256=%s task_id=%d path=%q", c.SHA256, c.TaskID, rel)
			return
		}
		sum.AlreadyPresent++
		log.Printf("attachment_archive.already_present sha256=%s task_id=%d path=%q", c.SHA256, c.TaskID, rel)
		return
	}
	if action == actionCopy {
		sum.Copied++
		log.Printf("attachment_archive.copied sha256=%s task_id=%d path=%q size_bytes=%d", c.SHA256, c.TaskID, rel, c.SizeBytes)
	} else {
		sum.AlreadyPresent++
		log.Printf("attachment_archive.already_present sha256=%s task_id=%d path=%q", c.SHA256, c.TaskID, rel)
	}
	if err := r.writeManifests(c, rel); err != nil {
		r.fail(sum, c, &stageError{ReasonManifestFailed, "attachment_archive.manifest_failed", err})
		return
	}
	rec, err := r.api.RecordAttachmentArchive(ctx, c.SHA256, rel, c.SizeBytes)
	if err != nil {
		r.fail(sum, c, &stageError{ReasonRecordFailed, "attachment_archive.record_failed", err})
		return
	}
	sum.Recorded++
	log.Printf("attachment_archive.recorded sha256=%s task_id=%d location=%q hot_deleted=%t", c.SHA256, c.TaskID, rec.Location, rec.HotDeleted)
}

// stage makes the stored copy present and verified, and reports whether this
// run wrote it. A file already present with the candidate's size and sha is
// left untouched; a file with different bytes stops the candidate and is
// never overwritten.
func (r *runner) stage(ctx context.Context, c client.ArchiveCandidate, abs string) (string, error) {
	fi, err := os.Stat(abs)
	switch {
	case err == nil && !fi.Mode().IsRegular():
		return "", &stageError{ReasonFileMismatch, "attachment_archive.verify_failed", fmt.Errorf("%s is not a regular file", abs)}
	case err == nil:
		ok, err := verifyFile(abs, c.SizeBytes, c.SHA256)
		if err != nil {
			return "", &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
		}
		if !ok {
			return "", &stageError{ReasonFileMismatch, "attachment_archive.verify_failed", fmt.Errorf("stored %s does not match sha256 %s (%d bytes)", abs, c.SHA256, c.SizeBytes)}
		}
		return actionPresent, nil
	case errors.Is(err, fs.ErrNotExist):
		if !r.opts.Apply {
			return actionCopy, nil
		}
		if err := r.copyAndVerify(ctx, c, abs); err != nil {
			return "", err
		}
		return actionCopy, nil
	default:
		return "", &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
}

// copyAndVerify downloads the candidate, writes it beside its final path,
// fsyncs, renames, then reads the renamed file back and checks size and sha.
// A copy that fails its read-back is removed so a later run retries it.
func (r *runner) copyAndVerify(ctx context.Context, c client.ArchiveCandidate, abs string) error {
	_, body, err := r.api.GetAttachment(ctx, c.SHA256)
	if err != nil {
		return &stageError{ReasonDownloadFailed, "attachment_archive.download_failed", err}
	}
	defer body.Close()
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
	tmp, err := os.CreateTemp(dir, ".hk-"+c.SHA256[:12]+"-*")
	if err != nil {
		return &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
	tmpName := tmp.Name()
	removeTemp := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		removeTemp()
		return &stageError{ReasonDownloadFailed, "attachment_archive.download_failed", err}
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != c.SizeBytes || got != c.SHA256 {
		removeTemp()
		return &stageError{ReasonVerifyFailed, "attachment_archive.verify_failed", fmt.Errorf("downloaded %d bytes sha %s, want %d bytes sha %s", n, got, c.SizeBytes, c.SHA256)}
	}
	if err := tmp.Sync(); err != nil {
		removeTemp()
		return &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return &stageError{ReasonWriteFailed, "attachment_archive.write_failed", err}
	}
	ok, err := verifyFile(abs, c.SizeBytes, c.SHA256)
	if err == nil && ok {
		return nil
	}
	os.Remove(abs)
	if err == nil {
		err = errors.New("the stored copy does not match the candidate")
	}
	return &stageError{ReasonVerifyFailed, "attachment_archive.verify_failed", err}
}

// writeManifests upserts the stored location into the manifest of the storage
// task and of every other task that refers to the sha.
func (r *runner) writeManifests(c client.ArchiveCandidate, rel string) error {
	ids := []int64{c.TaskID}
	seen := map[int64]bool{c.TaskID: true}
	for _, t := range c.Tasks {
		if t.ID > 0 && !seen[t.ID] {
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
	}
	entry := ManifestEntry{
		SHA256:       c.SHA256,
		Size:         c.SizeBytes,
		MIME:         c.MIME,
		OriginalName: c.OriginalName,
		Path:         rel,
		ArchivedAt:   r.now(),
		Source:       "hk",
	}
	for _, id := range ids {
		dir := filepath.Join(r.opts.Root, filepath.FromSlash(taskDir(id)))
		if err := upsertManifest(dir, entry); err != nil {
			return err
		}
		log.Printf("attachment_archive.manifest_written task_id=%d sha256=%s path=%q", id, c.SHA256, rel)
	}
	return nil
}

func (r *runner) fail(sum *Summary, c client.ArchiveCandidate, e *stageError) {
	sum.Failed = append(sum.Failed, Failure{SHA256: c.SHA256, TaskID: c.TaskID, Reason: e.reason, Detail: e.err.Error()})
	log.Printf("%s sha256=%s task_id=%d err=%q", e.event, c.SHA256, c.TaskID, e.err)
}

// stageError names why one candidate stopped and the log event that reports
// it. The event is a literal chosen at the call site, never built at run time.
type stageError struct {
	reason string
	event  string
	err    error
}

func (e *stageError) Error() string { return e.reason + ": " + e.err.Error() }

// checkRoot refuses a root that is not an existing directory or lacks the
// operator's marker file, so a missing Drive mount never takes the copies
// onto the local disk.
func checkRoot(root string) error {
	if root == "" {
		return errors.New("archive_root_missing: --root is required")
	}
	fi, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("archive_root_unusable: %v", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("archive_root_unusable: %s is not a directory", root)
	}
	mi, err := os.Stat(filepath.Join(root, MarkerFile))
	if err != nil || !mi.Mode().IsRegular() {
		return fmt.Errorf("archive_root_unmarked: %s lacks the %s marker the operator places once in the Drive mount", root, MarkerFile)
	}
	return nil
}

// storedPath is the archive location relative to the root. The storage task's
// directory holds the bytes; every referring task points at this one path.
func storedPath(c client.ArchiveCandidate) string {
	return path.Join(taskDir(c.TaskID), "attachments", c.SHA256+"-"+safeName(c.OriginalName))
}

func taskDir(id int64) string {
	return path.Join(ArchiveDir, "tasks", strconv.FormatInt(id, 10))
}

// safeName folds an original name to the safe alphabet of a stored file name:
// ASCII letters, digits, ".", "-" and "_" stay, every other rune becomes "_",
// a name longer than maxNameBytes is cut, and an empty or all-dot name
// becomes "attachment".
func safeName(name string) string {
	var b []byte
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	s := string(b)
	if len(s) > maxNameBytes {
		s = s[:maxNameBytes]
	}
	if strings.Trim(s, ".") == "" {
		return "attachment"
	}
	return s
}

// validSHA reports whether sha is the 64 lowercase hex bytes hk stores.
func validSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := range len(sha) {
		if !(sha[i] >= '0' && sha[i] <= '9' || sha[i] >= 'a' && sha[i] <= 'f') {
			return false
		}
	}
	return true
}

// verifyFile streams p once and reports whether its byte count and sha256
// equal the wanted values.
func verifyFile(p string, wantSize int64, wantSHA string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return false, err
	}
	return n == wantSize && hex.EncodeToString(h.Sum(nil)) == wantSHA, nil
}
