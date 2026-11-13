package main

import "testing"

func TestParseDocOlderThan(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		days int
		bad  bool
	}{
		{arg: "30d", days: 30},
		{arg: "1d", days: 1},
		{arg: "", bad: true},
		{arg: "0d", bad: true},
		{arg: "30", bad: true},
		{arg: "30h", bad: true},
		{arg: "-30d", bad: true},
		{arg: "999999d", bad: true},
	} {
		days, err := parseDocOlderThan(tc.arg)
		if tc.bad {
			if err == nil {
				t.Errorf("parseDocOlderThan(%q) = %d, want error", tc.arg, days)
			}
			continue
		}
		if err != nil || days != tc.days {
			t.Errorf("parseDocOlderThan(%q) = %d, %v; want %d", tc.arg, days, err, tc.days)
		}
	}
}
