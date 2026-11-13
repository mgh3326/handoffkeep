package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// testDocPruneSHA is a well-formed attachment digest for the ref test.
var testDocPruneSHA = "ab" + strings.Repeat("0", 62)

// putPruneDoc writes one document and backdates its updated_at to age ago.
func putPruneDoc(t *testing.T, s *Store, key string, age time.Duration) Document {
	t.Helper()
	x, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "note", Body: "body of " + key, CreatedBy: "operator"})
	if err != nil {
		t.Fatalf("PutDocument(%s): %v", key, err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE documents SET updated_at=$1 WHERE id=$2`, time.Now().UTC().Add(-age), x.ID); err != nil {
		t.Fatal(err)
	}
	return x
}

func docExists(t *testing.T, s *Store, key string) bool {
	t.Helper()
	_, found, err := s.GetDocument(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestPruneDocumentsDeletesOnlyOldMwrWithApply covers the acceptance rules:
// a review/mwr- document older than 30 days is deleted only with apply, a
// review/<date>/ document of any age is never matched, a bound document is
// skipped and counted, and a dry run deletes nothing.
func TestPruneDocumentsDeletesOnlyOldMwrWithApply(t *testing.T) {
	s := schemaStore(t, "doc_prune")
	ctx := t.Context()
	const day = 24 * time.Hour
	old := putPruneDoc(t, s, "review/mwr-old", 40*day)
	bound := putPruneDoc(t, s, "review/mwr-bound", 40*day)
	putPruneDoc(t, s, "review/mwr-fresh", 1*day)
	putPruneDoc(t, s, "review/2026-11-01/design", 400*day)
	dated := putPruneDoc(t, s, "review/mwr-dated-attach", 40*day)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_article_bindings(document_id,tenant,article_id,project_id,origin,bound_by,bound_at) VALUES($1,'tenant','article-1','PROJ','import_verified','test',now())`, bound.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES($1,1,'text/plain','a.txt','operator',now())`, testDocPruneSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES($1,'document',$2)`, testDocPruneSHA, dated.Key); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(-30 * day)

	dry, err := s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || dry.Deleted != 0 {
		t.Fatalf("dry run = %+v, want no deletes", dry)
	}
	if dry.Matched != 3 || dry.SkippedBound != 1 || dry.Bytes == 0 {
		t.Fatalf("dry run counts = %+v, want matched 3, skipped_bound 1, bytes > 0", dry)
	}
	if !docExists(t, s, old.Key) || !docExists(t, s, dated.Key) {
		t.Fatal("dry run deleted a document")
	}

	applied, err := s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Deleted != 2 || applied.SkippedBound != 1 {
		t.Fatalf("apply = %+v, want deleted 2, skipped_bound 1", applied)
	}
	if docExists(t, s, old.Key) || docExists(t, s, dated.Key) {
		t.Fatal("apply left an old unbound review/mwr- document")
	}
	if !docExists(t, s, bound.Key) {
		t.Fatal("apply deleted a bound document")
	}
	if !docExists(t, s, "review/mwr-fresh") || !docExists(t, s, "review/2026-11-01/design") {
		t.Fatal("apply deleted a document that must never match")
	}
	var refs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM attachment_refs WHERE ref_kind='document' AND ref_id=$1`, dated.Key).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("attachment refs for pruned document = %d, want 0", refs)
	}
}

// TestPruneDocumentsRefusesOtherPrefixes keeps the prune to review/mwr-.
func TestPruneDocumentsRefusesOtherPrefixes(t *testing.T) {
	s := schemaStore(t, "doc_prune_refuse")
	for _, prefix := range []string{"", "review/", "review/mwr", "review/2026-11-01/", "review/mwr-x/"} {
		_, err := s.PruneDocuments(t.Context(), prefix, time.Now().UTC().Add(-time.Hour), 10, true)
		if !errors.Is(err, ErrDocPruneInvalid) {
			t.Fatalf("PruneDocuments(%q) error = %v, want ErrDocPruneInvalid", prefix, err)
		}
	}
}

// TestPruneDocumentsRespectsLimit deletes at most limit documents per run.
func TestPruneDocumentsRespectsLimit(t *testing.T) {
	s := schemaStore(t, "doc_prune_limit")
	for _, key := range []string{"review/mwr-l1", "review/mwr-l2", "review/mwr-l3"} {
		putPruneDoc(t, s, key, 40*24*time.Hour)
	}
	res, err := s.PruneDocuments(t.Context(), DocPrunePrefix, time.Now().UTC().Add(-30*24*time.Hour), 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.Matched != 3 {
		t.Fatalf("limit 2 apply = %+v, want deleted 2 of matched 3", res)
	}
}
