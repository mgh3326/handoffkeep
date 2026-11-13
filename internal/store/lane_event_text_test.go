package store

import (
	"strings"
	"testing"
)

func TestLaneEventTextHasNoByteCeiling(t *testing.T) {
	text := strings.Repeat("diagnostic ", 4096)
	if !validLaneEventText(text) {
		t.Fatal("valid long lane-event text was refused")
	}
}

func TestAssistantLaneTextRetainsTheWholeNotification(t *testing.T) {
	body := strings.Repeat("한글 notification ", 512)
	want := "[hk] " + body
	if got := assistantLaneText("[hk] ", body); got != want {
		t.Fatalf("assistantLaneText kept %d bytes, want %d", len(got), len(want))
	}
}
