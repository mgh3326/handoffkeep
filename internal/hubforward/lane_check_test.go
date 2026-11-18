package hubforward

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestLaneCheckForwardsAsInfo pins the hub type of a lane_check notice: a wake
// the design session judges, never an interrupt (operator decision 418,
// hk 5201).
func TestLaneCheckForwardsAsInfo(t *testing.T) {
	if got := notifyType(store.OutboxKindLaneCheck); got != "info" {
		t.Fatalf("lane_check type = %q, want info", got)
	}
}
