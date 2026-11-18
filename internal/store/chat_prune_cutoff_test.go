package store

import (
	"testing"
	"time"
)

// The prune cutoff is one retention year before now and must match the
// PostgreSQL now() - interval '1 year' and MySQL DATE_SUB(..., INTERVAL 1 YEAR)
// on every day, including Feb 29.
func TestChatPruneCutoffMatchesIntervalArithmetic(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"leap day clamps to end of February", time.Date(2028, time.February, 29, 12, 34, 56, 123000000, time.UTC), time.Date(2027, time.February, 28, 12, 34, 56, 123000000, time.UTC)},
		{"leap day of another leap year", time.Date(2032, time.February, 29, 0, 0, 0, 0, time.UTC), time.Date(2031, time.February, 28, 0, 0, 0, 0, time.UTC)},
		{"day after leap day", time.Date(2028, time.March, 1, 9, 0, 0, 0, time.UTC), time.Date(2027, time.March, 1, 9, 0, 0, 0, time.UTC)},
		{"ordinary February 28", time.Date(2028, time.February, 28, 23, 59, 59, 0, time.UTC), time.Date(2027, time.February, 28, 23, 59, 59, 0, time.UTC)},
		{"non-UTC input is converted first", time.Date(2028, time.February, 29, 1, 0, 0, 0, time.FixedZone("KST", 9*3600)), time.Date(2027, time.February, 28, 16, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatPruneCutoff(tc.now); !got.Equal(tc.want) {
				t.Fatalf("chatPruneCutoff(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
