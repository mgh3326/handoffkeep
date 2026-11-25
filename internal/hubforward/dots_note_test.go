package hubforward

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestDotsNoteForwardsAsInfo pins the hub type of a dots_note: dots' opinion
// for the design session to sort, never a wake-asking type (hk 6307).
func TestDotsNoteForwardsAsInfo(t *testing.T) {
	if store.OutboxKindDotsNote != "dots_note" {
		t.Fatalf("OutboxKindDotsNote = %q, want dots_note", store.OutboxKindDotsNote)
	}
	if got := notifyType(store.OutboxKindDotsNote); got != "info" {
		t.Fatalf("dots_note type = %q, want info", got)
	}
}
