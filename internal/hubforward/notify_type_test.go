package hubforward

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestNotifyTypeByKind pins the hub type of every outbox kind (hk 6341): what
// a lane acts on at once carries a wake-asking type, the rest is info.
func TestNotifyTypeByKind(t *testing.T) {
	want := map[string]string{
		store.OutboxKindDecisionAnswered:    "decision",
		store.OutboxKindDecisionDenied:      "decision",
		store.OutboxKindChatAnswer:          "decision",
		store.OutboxKindDispositionAnswered: "decision",
		store.OutboxKindDotsDecision:        "decision",
		store.OutboxKindDotsBlocked:         "blocked",
		store.OutboxKindDotsReport:          "ready",
		store.OutboxKindTaskOrder:           "order",
		store.OutboxKindDeployResult:        "findings",
		store.OutboxKindLaneCheck:           "findings",
		store.OutboxKindTaskDrift:           "info",
		store.OutboxKindDeployLag:           "info",
		store.OutboxKindDotsNote:            "info",
		store.OutboxKindDotsFollowup:        "info",
		store.OutboxKindYTWriteOp:           "info",
	}
	for kind, typ := range want {
		if got := notifyType(kind); got != typ {
			t.Errorf("notifyType(%q) = %q, want %q", kind, got, typ)
		}
	}
}
