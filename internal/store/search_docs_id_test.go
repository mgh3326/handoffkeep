package store

import (
	"context"
	"fmt"
	"strconv"
	"testing"
)

// A "#<n>" or bare-number query in a scope that contains documents resolves
// the document id first, exactly like scope "tasks" resolves task ids: the
// exact match leads the page and a document whose key or body merely contains
// the digits must never outrank it (#551).
func TestSearchDocsExactIDLeads(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	target, _, err := s.PutDocument(ctx, Document{Key: "k/id-target", Kind: "note", Body: "plain body", CreatedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutDocument(ctx, Document{Key: "k/mentions-" + strconv.FormatInt(target.ID, 10), Kind: "note", Body: "body cites " + strconv.FormatInt(target.ID, 10), CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"#" + strconv.FormatInt(target.ID, 10), strconv.FormatInt(target.ID, 10)} {
		for _, scope := range []string{"docs", "all"} {
			xs, err := s.Search(ctx, q, scope, "", 10)
			if err != nil {
				t.Fatalf("q=%s scope=%s err=%v", q, scope, err)
			}
			if len(xs) == 0 || xs[0].Scope != "docs" || xs[0].Key != target.Key {
				t.Fatalf("q=%s scope=%s first=%+v", q, scope, resultKeys(xs))
			}
			pos := -1
			for i, x := range xs {
				if x.Key == "k/mentions-"+strconv.FormatInt(target.ID, 10) {
					pos = i
				}
			}
			if pos <= 0 {
				t.Fatalf("q=%s scope=%s mention position=%d keys=%v", q, scope, pos, resultKeys(xs))
			}
		}
	}
	// A document whose own body repeats its id appears exactly once.
	if _, _, err = s.PutDocument(ctx, Document{Key: "k/id-target", Kind: "note", Body: "self cites " + strconv.FormatInt(target.ID, 10), CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	xs, err := s.Search(ctx, strconv.FormatInt(target.ID, 10), "docs", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, x := range xs {
		if x.Key == target.Key {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("exact doc duplicated: %v", resultKeys(xs))
	}
	// Missing and oversized ids yield an empty result set, not body matches.
	for _, q := range []string{"999999999", "#999999999", "99999999999999999999999"} {
		xs, err = s.Search(ctx, q, "docs", "", 10)
		if err != nil || len(xs) != 0 {
			t.Fatalf("q=%s xs=%v err=%v", q, resultKeys(xs), err)
		}
	}
	// The session filter applies to the id lookup too.
	xs, err = s.Search(ctx, strconv.FormatInt(target.ID, 10), "docs", "other-session", 10)
	if err != nil || len(xs) != 0 {
		t.Fatalf("session-filtered id lookup xs=%v err=%v", resultKeys(xs), err)
	}
	// Non-numeric queries keep the plain FTS path.
	xs, err = s.Search(ctx, "body cites", "docs", "", 10)
	if err != nil || len(xs) != 1 || xs[0].Key != "k/mentions-"+strconv.FormatInt(target.ID, 10) {
		t.Fatalf("fts xs=%v err=%v", resultKeys(xs), err)
	}
}

// Scopes other than tasks also mark a page cut at the cap: every returned row
// carries truncated=true, and a page that fits reports no truncation (#551).
func TestSearchScopesCapAndTruncated(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		if _, err := s.CreateCheckpoint(ctx, Checkpoint{Session: "ss", Kind: "checkpoint", Title: fmt.Sprintf("capprobe %d", i), Body: "x", CreatedBy: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	xs, err := s.Search(ctx, "capprobe", "ctx", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 5 {
		t.Fatalf("limit=5 got %d", len(xs))
	}
	for _, x := range xs {
		if !x.Truncated {
			t.Fatal("truncated page must mark every row")
		}
	}
	xs, err = s.Search(ctx, "capprobe", "ctx", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 7 {
		t.Fatalf("limit=10 got %d", len(xs))
	}
	for _, x := range xs {
		if x.Truncated {
			t.Fatal("complete page must not claim truncation")
		}
	}
	// An exact-fit page (rows == limit) is not truncation.
	xs, err = s.Search(ctx, "capprobe", "ctx", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 7 || xs[0].Truncated {
		t.Fatalf("exact-fit page: n=%d truncated=%v", len(xs), xs[0].Truncated)
	}
	// The default page is 20 and the upper bound stays 100.
	for i := 0; i < 25; i++ {
		if _, _, err = s.PutDocument(ctx, Document{Key: fmt.Sprintf("k/capfill-%02d", i), Kind: "note", Body: "capfill body", CreatedBy: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	xs, err = s.Search(ctx, "capfill", "docs", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 20 || !xs[0].Truncated {
		t.Fatalf("default page got %d truncated=%v", len(xs), len(xs) > 0 && xs[0].Truncated)
	}
	xs, err = s.Search(ctx, "capfill", "docs", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 25 || xs[0].Truncated {
		t.Fatalf("limit=100 got %d truncated=%v", len(xs), xs[0].Truncated)
	}
}

func TestGetDocumentByID(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	put, _, err := s.PutDocument(ctx, Document{Key: "k/by-id", Kind: "note", Body: "by id body", CreatedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	x, found, err := s.GetDocumentByID(ctx, put.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if x.Key != "k/by-id" || x.Body != "by id body" || x.ID != put.ID {
		t.Fatalf("doc=%+v", x)
	}
	// By key still resolves the same row.
	byKey, found, err := s.GetDocument(ctx, "k/by-id")
	if err != nil || !found || byKey.ID != put.ID {
		t.Fatalf("by key found=%v id=%d err=%v", found, byKey.ID, err)
	}
	if _, found, err = s.GetDocumentByID(ctx, 999999999); err != nil || found {
		t.Fatalf("missing id found=%v err=%v", found, err)
	}
	for _, id := range []int64{0, -5} {
		if _, _, err = s.GetDocumentByID(ctx, id); err == nil {
			t.Fatalf("id=%d must be rejected", id)
		}
	}
}
