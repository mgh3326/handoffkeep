package store

import (
	"testing"
	"time"
)

func TestSLARisk(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	medium, high, breach := start.Add(time.Hour), start.Add(2*time.Hour), start.Add(3*time.Hour)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{start.Add(-time.Second), "none"}, {start, "none"},
		{medium, "medium"}, {high, "high"}, {breach, "breached"},
	} {
		if got := slaRisk(tc.at, &start, &medium, &high, &breach); got != tc.want {
			t.Errorf("at %s: got %s want %s", tc.at, got, tc.want)
		}
	}
	if got := slaRisk(breach, nil, &medium, &high, &breach); got != "none" {
		t.Errorf("unset SLA: got %s", got)
	}
}
