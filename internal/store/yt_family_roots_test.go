package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ytFamilyOp prepares the article_create op a family root record names: the
// table's op_id is a foreign key into the op log.
func ytFamilyOp(t *testing.T, l ytwrite.Ledger, key string) ytwrite.Op {
	t.Helper()
	op, _, err := l.Prepare(t.Context(), time.Now().UTC(), ytwrite.NewOp{
		Principal: "p", Kind: ytwrite.KindArticleCreate, Key: key, PayloadSHA: "sha-" + key,
		Payload: []byte(`{}`), EntityKind: ytwrite.ArticleEntity, EntityID: "1",
		YTTargetID: "0-30", Tenant: "yt.example", Actor: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// One root per family and project, first record wins, and each project's (and
// tenant's) family is its own (hk 4797).
func TestYTFamilyRootsRecordOncePerFamily(t *testing.T) {
	s := schemaStore(t, "ytfr")
	ctx := context.Background()
	l := s.YTWriteLedger()
	op := ytFamilyOp(t, l, "k1")
	if _, found, err := l.FamilyRoot(ctx, "yt.example", "0-30", "design"); err != nil || found {
		t.Fatalf("empty record: found %v err %v", found, err)
	}
	if err := l.RecordFamilyRoot(ctx, "yt.example", "0-30", "design", "3-1", op.ID); err != nil {
		t.Fatal(err)
	}
	// A second document of the family resolved the same root, or a racing one
	// resolved another: the recorded root stays the family's.
	if err := l.RecordFamilyRoot(ctx, "yt.example", "0-30", "design", "3-2", op.ID); err != nil {
		t.Fatal(err)
	}
	got, found, err := l.FamilyRoot(ctx, "yt.example", "0-30", "design")
	if err != nil || !found || got != "3-1" {
		t.Fatalf("recorded root %q found %v err %v, want 3-1", got, found, err)
	}
	for _, other := range []struct{ tenant, project, family string }{
		{"yt.example", "0-31", "design"},
		{"other.example", "0-30", "design"},
		{"yt.example", "0-30", "briefs"},
	} {
		if _, found, err := l.FamilyRoot(ctx, other.tenant, other.project, other.family); err != nil || found {
			t.Fatalf("%+v read a record of another family: found %v err %v", other, found, err)
		}
	}
}

// The family lane excludes a second writer across connections, and is free
// again once released; another family is never held by it (hk 4797).
func TestYTFamilyLaneIsExclusive(t *testing.T) {
	s := schemaStore(t, "ytfl")
	ctx := context.Background()
	l := s.YTWriteLedger()
	unlock, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "design")
	if err != nil || !ok || unlock == nil {
		t.Fatalf("first lane: ok %v unlock %v err %v", ok, unlock != nil, err)
	}
	if _, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "design"); err != nil || ok {
		t.Fatalf("second writer took the held lane: ok %v err %v", ok, err)
	}
	otherUnlock, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "briefs")
	if err != nil || !ok {
		t.Fatalf("another family of the same project: ok %v err %v", ok, err)
	}
	otherUnlock()
	unlock()
	unlock2, ok, err := l.LockFamily(ctx, "yt.example", "0-30", "design")
	if err != nil || !ok {
		t.Fatalf("lane not free after release: ok %v err %v", ok, err)
	}
	unlock2()
}
