package api

import (
	"strings"
	"testing"
)

func TestDecisionResolveTextHasNoByteCeiling(t *testing.T) {
	if !validDecisionResolveText(strings.Repeat("operator reason ", 512), true) {
		t.Fatal("long decision answer was refused")
	}
}
