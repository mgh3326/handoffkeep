package attachmentarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/client"
)

// fakeAPI serves fixed candidates and bodies, and records the calls a run
// made. pageSize > 0 splits the candidates into pages of that size.
type fakeAPI struct {
	candidates []client.ArchiveCandidate
	bodies     map[string][]byte
	pageSize   int
	recordErr  error

	calls    []string
	onRecord func(sha, location string)
}

func (f *fakeAPI) ArchiveCandidates(_ context.Context, _ int, after string) (client.ArchiveCandidates, error) {
	f.calls = append(f.calls, "candidates")
	start := 0
	if after != "" {
		for i, c := range f.candidates {
			if c.SHA256 == after {
				start = i + 1
			}
		}
	}
	size := f.pageSize
	if size <= 0 || size > len(f.candidates)-start {
		size = len(f.candidates) - start
	}
	end := start + size
	page := client.ArchiveCandidates{Candidates: f.candidates[start:end]}
	if end < len(f.candidates) {
		page.Next = f.candidates[end-1].SHA256
	}
	return page, nil
}

func (f *fakeAPI) GetAttachment(_ context.Context, sha string) (client.Attachment, io.ReadCloser, error) {
	f.calls = append(f.calls, "get:"+sha)
	b, ok := f.bodies[sha]
	if !ok {
		return client.Attachment{}, nil, errors.New("no body for " + sha)
	}
	return client.Attachment{SHA256: sha, SizeBytes: int64(len(b)), MIME: "text/plain"}, io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeAPI) RecordAttachmentArchive(_ context.Context, sha, location string, _ int64) (client.AttachmentArchive, error) {
	f.calls = append(f.calls, "record:"+sha)
	if f.onRecord != nil {
		f.onRecord(sha, location)
	}
	if f.recordErr != nil {
		return client.AttachmentArchive{}, f.recordErr
	}
	return client.AttachmentArchive{Location: location, ArchivedAt: time.Now().UTC()}, nil
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// archiveRoot is a temp directory carrying the operator's marker file.
func archiveRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MarkerFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// candidate builds one candidate whose body hashes to its sha.
func candidate(body []byte, taskID int64, tasks ...int64) client.ArchiveCandidate {
	if len(tasks) == 0 {
		tasks = []int64{taskID}
	}
	closed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	refs := make([]client.ArchiveCandidateTask, 0, len(tasks))
	for _, id := range tasks {
		refs = append(refs, client.ArchiveCandidateTask{ID: id, State: "completed", ClosedAt: closed})
	}
	return client.ArchiveCandidate{
		SHA256:       shaHex(body),
		SizeBytes:    int64(len(body)),
		MIME:         "text/plain",
		OriginalName: "note.txt",
		TaskID:       taskID,
		Tasks:        refs,
	}
}

// filesUnder lists every regular file under dir, as slash paths relative to
// dir, sorted.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readManifest(t *testing.T, p string) Manifest {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func manifestPath(root string, taskID int64) string {
	return filepath.Join(root, filepath.FromSlash(path.Join(taskDir(taskID), ManifestName)))
}

// A dry run reports what it would write and touches nothing.
func TestDryRunWritesNothing(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("dry run body")
	c := candidate(body, 42)
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: map[string][]byte{c.SHA256: body}}
	sum, err := Archive(context.Background(), api, Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.DryRun || sum.Candidates != 1 || sum.Copied != 0 || sum.AlreadyPresent != 0 || sum.Recorded != 0 || len(sum.Failed) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	want := path.Join(ArchiveDir, "tasks", "42", "attachments", c.SHA256+"-note.txt")
	if len(sum.Planned) != 1 || sum.Planned[0].Path != want || sum.Planned[0].TaskID != 42 {
		t.Fatalf("planned = %+v, want %s", sum.Planned, want)
	}
	if got := filesUnder(t, dir); len(got) != 1 || got[0] != MarkerFile {
		t.Fatalf("dry run wrote %v", got)
	}
	for _, call := range api.calls {
		if strings.HasPrefix(call, "get:") || strings.HasPrefix(call, "record:") {
			t.Fatalf("dry run made call %q", call)
		}
	}
}

// With --apply a run copies and verifies the file, writes the manifest, and
// only then records; the manifest carries the design's fields.
func TestApplyCopiesManifestsThenRecords(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("copy me")
	c := candidate(body, 7)
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: map[string][]byte{c.SHA256: body}}
	var recorded bool
	api.onRecord = func(sha, location string) {
		recorded = true
		ok, err := verifyFile(filepath.Join(dir, filepath.FromSlash(location)), c.SizeBytes, sha)
		if err != nil || !ok {
			t.Fatalf("recorded before the stored copy verified: ok=%t err=%v", ok, err)
		}
		m := readManifest(t, manifestPath(dir, 7))
		if len(m.Attachments) != 1 || m.Attachments[0].Path != location {
			t.Fatalf("recorded before the manifest named %s: %+v", location, m)
		}
	}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("record was never called")
	}
	if sum.Copied != 1 || sum.Recorded != 1 || sum.AlreadyPresent != 0 || len(sum.Failed) != 0 || sum.DryRun {
		t.Fatalf("summary = %+v", sum)
	}
	wantCalls := []string{"candidates", "get:" + c.SHA256, "record:" + c.SHA256}
	if !reflect.DeepEqual(api.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", api.calls, wantCalls)
	}
	e := readManifest(t, manifestPath(dir, 7)).Attachments[0]
	if e.SHA256 != c.SHA256 || e.Size != c.SizeBytes || e.MIME != "text/plain" || e.OriginalName != "note.txt" || e.Source != "hk" || e.Path != storedPath(c) || e.ArchivedAt.IsZero() {
		t.Fatalf("manifest entry = %+v", e)
	}
}

// A sha two closed tasks refer to is stored once, and both manifests name
// that one path.
func TestSharedSHABothManifestsPointAtOneFile(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("shared bytes")
	c := candidate(body, 100, 100, 101)
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: map[string][]byte{c.SHA256: body}}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.Recorded != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	stored := storedPath(c)
	files := filesUnder(t, dir)
	want := []string{
		MarkerFile,
		path.Join(ArchiveDir, "tasks", "100", "manifest.json"),
		path.Join(ArchiveDir, "tasks", "101", "manifest.json"),
		stored,
	}
	sort.Strings(want)
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	for _, id := range []int64{100, 101} {
		m := readManifest(t, manifestPath(dir, id))
		if len(m.Attachments) != 1 || m.Attachments[0].Path != stored {
			t.Fatalf("task %d manifest = %+v, want one entry at %s", id, m, stored)
		}
	}
}

// A stored copy with the candidate's sha is not downloaded or rewritten.
func TestAlreadyPresentFileIsNotRewritten(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("already stored")
	c := candidate(body, 5)
	abs := filepath.Join(dir, filepath.FromSlash(storedPath(c)))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.AlreadyPresent != 1 || sum.Copied != 0 || sum.Recorded != 1 || len(sum.Failed) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	after, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the stored file was rewritten")
	}
	for _, call := range api.calls {
		if strings.HasPrefix(call, "get:") {
			t.Fatalf("an already present file was downloaded: %q", call)
		}
	}
	if m := readManifest(t, manifestPath(dir, 5)); len(m.Attachments) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
}

// A stored file whose bytes differ from the candidate stops that sha: it is
// neither overwritten nor recorded, and no manifest is written.
func TestStoredFileMismatchIsNotRecorded(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("wanted bytes")
	c := candidate(body, 5)
	abs := filepath.Join(dir, filepath.FromSlash(storedPath(c)))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failed) != 1 || sum.Failed[0].Reason != ReasonFileMismatch || sum.Failed[0].SHA256 != c.SHA256 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Recorded != 0 {
		t.Fatal("a mismatched sha was recorded")
	}
	for _, call := range api.calls {
		if strings.HasPrefix(call, "record:") {
			t.Fatalf("a mismatched sha was recorded: %q", call)
		}
	}
	got, err := os.ReadFile(abs)
	if err != nil || string(got) != "other bytes" {
		t.Fatalf("the mismatched file changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(manifestPath(dir, 5)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a manifest was written for a mismatched sha: %v", err)
	}
}

// Bytes that do not hash to the candidate are removed and never recorded.
func TestDownloadedBytesThatDoNotMatchAreNotRecorded(t *testing.T) {
	dir := archiveRoot(t)
	c := candidate([]byte("declared bytes"), 9)
	api := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: map[string][]byte{c.SHA256: []byte("served bytes")}}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failed) != 1 || sum.Failed[0].Reason != ReasonVerifyFailed {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Recorded != 0 || sum.Copied != 0 {
		t.Fatalf("a mismatched download counted as archived: %+v", sum)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(storedPath(c)))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the mismatched copy was kept: %v", err)
	}
	for _, call := range api.calls {
		if strings.HasPrefix(call, "record:") {
			t.Fatalf("a mismatched download was recorded: %q", call)
		}
	}
}

// A root that is missing, not a directory, or lacks the marker is refused
// before any candidate is read or written.
func TestRootWithoutMarkerOrDirectoryIsRefused(t *testing.T) {
	body := []byte("x")
	c := candidate(body, 1)
	cases := []struct {
		name string
		root string
		want string
	}{
		{"empty", "", "--root is required"},
		{"missing", filepath.Join(t.TempDir(), "absent"), "archive_root_unusable"},
		{"file", filepath.Join(t.TempDir(), "plain"), "archive_root_unusable"},
		{"no marker", t.TempDir(), MarkerFile},
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases[2].root = plain
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: map[string][]byte{c.SHA256: body}}
			_, err := Archive(context.Background(), api, Options{Root: tc.root, Apply: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			if len(api.calls) != 0 {
				t.Fatalf("a refused root read candidates: %v", api.calls)
			}
		})
	}
}

// A run that failed at the record leaves the copy and the manifest; the next
// run finds them, records again, and rewrites neither.
func TestRerunAfterACrashBetweenCopyAndRecordConverges(t *testing.T) {
	dir := archiveRoot(t)
	body := []byte("crash between copy and record")
	c := candidate(body, 11, 11, 12)
	bodies := map[string][]byte{c.SHA256: body}
	abs := filepath.Join(dir, filepath.FromSlash(storedPath(c)))

	first := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: bodies, recordErr: errors.New("network down")}
	sum, err := Archive(context.Background(), first, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.Recorded != 0 || len(sum.Failed) != 1 || sum.Failed[0].Reason != ReasonRecordFailed {
		t.Fatalf("first run summary = %+v", sum)
	}
	fileBefore, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(manifestPath(dir, 11))
	if err != nil {
		t.Fatal(err)
	}

	second := &fakeAPI{candidates: []client.ArchiveCandidate{c}, bodies: bodies}
	sum, err = Archive(context.Background(), second, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || sum.AlreadyPresent != 1 || sum.Recorded != 1 || len(sum.Failed) != 0 {
		t.Fatalf("second run summary = %+v", sum)
	}
	fileAfter, err := os.ReadFile(abs)
	if err != nil || !bytes.Equal(fileAfter, fileBefore) {
		t.Fatalf("the stored copy changed on the rerun: %q err=%v", fileAfter, err)
	}
	manifestAfter, err := os.ReadFile(manifestPath(dir, 11))
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("the manifest changed on the rerun: %s err=%v", manifestAfter, err)
	}
	for _, task := range []int64{11, 12} {
		if m := readManifest(t, manifestPath(dir, task)); len(m.Attachments) != 1 || m.Attachments[0].Path != storedPath(c) {
			t.Fatalf("task %d manifest = %+v", task, m)
		}
	}
	if got := filesUnder(t, dir); len(got) != 4 {
		t.Fatalf("files = %v, want one copy and two manifests beside the marker", got)
	}
}

// The run pages through the candidates and stops at --limit.
func TestPagingAndLimit(t *testing.T) {
	dir := archiveRoot(t)
	var cs []client.ArchiveCandidate
	bodies := map[string][]byte{}
	for _, b := range [][]byte{[]byte("one"), []byte("two"), []byte("three")} {
		c := candidate(b, 20)
		cs = append(cs, c)
		bodies[c.SHA256] = b
	}
	api := &fakeAPI{candidates: cs, bodies: bodies, pageSize: 1}
	sum, err := Archive(context.Background(), api, Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Candidates != 3 || len(sum.Planned) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := strings.Count(strings.Join(api.calls, ","), "candidates"); got != 3 {
		t.Fatalf("candidates calls = %d, want 3", got)
	}

	api = &fakeAPI{candidates: cs, bodies: bodies, pageSize: 1}
	sum, err = Archive(context.Background(), api, Options{Root: dir, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Candidates != 2 || len(sum.Planned) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
}

// A candidate with a sha or task id that cannot name a path is refused
// without a download.
func TestUnusableCandidateIsRefused(t *testing.T) {
	dir := archiveRoot(t)
	bad := client.ArchiveCandidate{SHA256: "../../etc/passwd", SizeBytes: 1, OriginalName: "x", TaskID: 3, Tasks: []client.ArchiveCandidateTask{{ID: 3}}}
	noTask := candidate([]byte("x"), 0)
	api := &fakeAPI{candidates: []client.ArchiveCandidate{bad, noTask}}
	sum, err := Archive(context.Background(), api, Options{Root: dir, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failed) != 2 || sum.Failed[0].Reason != ReasonCandidateRefused || sum.Failed[1].Reason != ReasonCandidateRefused {
		t.Fatalf("summary = %+v", sum)
	}
	if len(api.calls) != 1 {
		t.Fatalf("calls = %v, want only the candidates read", api.calls)
	}
}

func TestSafeName(t *testing.T) {
	long := strings.Repeat("a", 300)
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"a/b c?.txt", "a_b_c_.txt"},
		{"한글.txt", "__.txt"},
		{"C:\\x.png", "C__x.png"},
		{"", "attachment"},
		{"...", "attachment"},
		{long, strings.Repeat("a", maxNameBytes)},
	}
	for _, tc := range cases {
		if got := safeName(tc.in); got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
