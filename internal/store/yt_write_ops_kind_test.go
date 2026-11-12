package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The operator's view filters by kind; an unknown kind is refused, not an
// empty page.
func TestListYTWriteOpsFiltersByKind(t *testing.T) {
	s := schemaStore(t, "ytwk")
	ctx := t.Context()
	l := s.YTWriteLedger()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if _, _, err := l.Prepare(ctx, now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: "k1", PayloadSHA: "s1",
		Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "1", YTTargetID: "2-1", Actor: "a"}); err != nil {
		t.Fatal(err)
	}

	comments, err := s.ListYTWriteOps(ctx, YTOpFilter{State: "all", Kind: ytwrite.KindCommentAppend, Limit: 10})
	if err != nil || len(comments) != 1 || comments[0].Kind != ytwrite.KindCommentAppend {
		t.Fatalf("comment_append filter = %+v, %v; want the one comment op", comments, err)
	}
	fields, err := s.ListYTWriteOps(ctx, YTOpFilter{State: "all", Kind: ytwrite.KindIssueFields, Limit: 10})
	if err != nil || len(fields) != 0 {
		t.Fatalf("issue_fields filter = %+v, %v; want none", fields, err)
	}
	if _, err := s.ListYTWriteOps(ctx, YTOpFilter{State: "all", Kind: "not_a_kind", Limit: 10}); !errors.Is(err, ytwrite.ErrInvalid) {
		t.Fatalf("unknown kind = %v, want ErrInvalid", err)
	}
}
