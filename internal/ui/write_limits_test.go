package ui

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestDecisionBatchParsesEverySubmittedItem(t *testing.T) {
	const count = 1200
	values := make(url.Values, count*3)
	for i := 0; i < count; i++ {
		prefix := "items." + strconv.Itoa(i) + "."
		values.Set(prefix+"type", "lane")
		values.Set(prefix+"id", strconv.Itoa(i+1))
		values.Set(prefix+"answer", "approve")
	}
	items, submitted, err := parseDecisionBatchItems(values)
	if err != nil || len(items) != count || submitted != count {
		t.Fatalf("items=%d submitted=%d err=%v, want %d", len(items), submitted, err, count)
	}
}

func TestConsoleWriteTextHasNoLaneEventByteCeiling(t *testing.T) {
	if !validWriteText(strings.Repeat("operator message ", 512)) {
		t.Fatal("long console message was refused")
	}
}
