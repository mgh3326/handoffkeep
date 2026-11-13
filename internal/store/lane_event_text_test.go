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
