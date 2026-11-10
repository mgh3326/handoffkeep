package store

import (
	"context"
	"strings"
	"testing"
)

func TestValidActor(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"client-name", true},
		{"operator:someone@example.com", true},
		{"", false},
		{"operator:", false},
		{"operator:someone@example.com\x00", false},
		{"operator:" + strings.Repeat("a", 121), false},
	} {
		if got := ValidActor(tc.value); got != tc.valid {
			t.Errorf("ValidActor(%q)=%v, want %v", tc.value, got, tc.valid)
		}
	}
}

func TestActorWriteRefusalsNameField(t *testing.T) {
	ctx := context.Background()
	tooLong := strings.Repeat("a", 129)
	for _, actor := range []string{"bad\x00actor", tooLong} {
		t.Run(actor, func(t *testing.T) {
			if _, err := (*Store)(nil).CreateCheckpoint(ctx, Checkpoint{Session: "session", Kind: "checkpoint", Title: "title", CreatedBy: actor}); err == nil || !strings.Contains(err.Error(), "created_by") {
				t.Fatalf("CreateCheckpoint error=%v, want created_by refusal", err)
			}
			if _, err := (*Store)(nil).PutMemory(ctx, Memory{Agent: "agent", Name: "memory", Type: "user", UpdatedBy: actor}); err == nil || !strings.Contains(err.Error(), "updated_by") {
				t.Fatalf("PutMemory error=%v, want updated_by refusal", err)
			}
			if _, _, err := (*Store)(nil).PutDocument(ctx, Document{Key: "actor-test", Kind: "note", CreatedBy: actor}); err == nil || !strings.Contains(err.Error(), "created_by") {
				t.Fatalf("PutDocument error=%v, want created_by refusal", err)
			}
			if _, _, err := (*Store)(nil).PutAttachment(ctx, Attachment{SHA256: strings.Repeat("a", 64), MIME: "text/plain", OriginalName: "x.txt", CreatedBy: actor, RefKind: "none"}, 0, 0); err == nil || !strings.Contains(err.Error(), "created_by") {
				t.Fatalf("PutAttachment error=%v, want created_by refusal", err)
			}
		})
	}
}
