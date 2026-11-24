package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The length refusal runs before any database access, so this needs no store.
func TestPutDocumentRefusesBodyOverTheLimit(t *testing.T) {
	var s Store
	x := Document{Key: "k/oversized", Kind: "note", CreatedBy: "tester", Body: strings.Repeat("a", DocumentMaxBytes+1)}
	_, _, err := s.PutDocument(context.Background(), x)
	if !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("err=%v, want ErrDocumentTooLarge", err)
	}
	if got := strings.TrimPrefix(err.Error(), ErrDocumentTooLarge.Error()+": "); got == err.Error() || !strings.Contains(got, "1048577") {
		t.Fatalf("reason %q does not name the body length", got)
	}
}
