package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tsAt(day int, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

func TestTableStatsDueFollowsDailyHourAndMaxAge(t *testing.T) {
	cases := []struct {
		name   string
		now    time.Time
		newest time.Time
		found  bool
		want   bool
	}{
		{"no snapshot yet", tsAt(24, 1), time.Time{}, false, true},
		{"same UTC day, after the hour", tsAt(23, 20), tsAt(23, 3), true, false},
		{"next day before the hour, under a day old", tsAt(24, 2), tsAt(23, 3), true, false},
		{"next day at the hour", tsAt(24, 3), tsAt(23, 3), true, true},
		{"catch-up: newest a day old, before the hour", tsAt(24, 2).Add(30 * time.Minute), tsAt(23, 2), true, true},
		{"restart the same day, under a day old", tsAt(23, 3).Add(30 * time.Minute), tsAt(23, 3), true, false},
	}
	for _, c := range cases {
		if got := TableStatsDue(c.now, c.newest, c.found); got != c.want {
			t.Errorf("%s: TableStatsDue(%s) = %v, want %v", c.name, c.now.Format(time.RFC3339), got, c.want)
		}
	}
}

func tsRecord(at time.Time, tables map[string][4]int64) tableStatRecord {
	return tableStatRecord{At: at, ReadMS: 12, Fresh: true, Tables: tables}
}

func TestRenderTableStatsGrowthAgainstPreviousAndSevenDaysEarlier(t *testing.T) {
	h := tableStatsHistory{V: 1, Snapshots: []tableStatRecord{
		// Seven days before the newest, exactly; it is the reference.
		tsRecord(tsAt(16, 3), map[string][4]int64{"orders": {100, 100, 1000, 200}, "gone": {5, 5, 50, 5}}),
		// Three days before the newest: skipped, the reference is the one above.
		tsRecord(tsAt(20, 3), map[string][4]int64{"orders": {150, 150, 1500, 300}}),
		tsRecord(tsAt(22, 3), map[string][4]int64{"orders": {180, 180, 1800, 360}}),
		tsRecord(tsAt(23, 3), map[string][4]int64{"orders": {200, 200, 2000, 400}, "new_tbl": {7, 7, 70, 7}}),
	}}
	body, err := renderTableStatsDoc(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Previous snapshot: 2026-11-22T03:00:00Z. About seven days earlier: 2026-11-16T03:00:00Z.",
		// Now: 207 rows, 2477 bytes (orders 2400 + new_tbl 77). Previous (d22):
		// 180 rows, 2160 bytes. Seven days earlier (d16): 105 rows, 1255 bytes.
		"Totals: 207 rows (+27 vs previous, +102 vs about seven days earlier); 2477 data and index bytes (+317, +1222).",
		"Tables added since the previous snapshot: 1. Removed: 0.",
		"| `orders` | 200 | 200 | 2000 | 400 | +20 | +240 | +100 | +1200 |",
		"| `new_tbl` | 7 | 7 | 70 | 7 | new | new | new | new |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered body misses %q\n%s", want, body)
		}
	}
	if !strings.Contains(body, "## History") || !strings.Contains(body, tableStatsFence) {
		t.Fatalf("history block missing:\n%s", body)
	}
}

func TestRenderTableStatsWithoutReferenceShowsNotYet(t *testing.T) {
	h := tableStatsHistory{V: 1, Snapshots: []tableStatRecord{
		tsRecord(tsAt(23, 3), map[string][4]int64{"orders": {1, 1, 10, 1}}),
	}}
	body, err := renderTableStatsDoc(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Previous snapshot: none yet. About seven days earlier: none yet.",
		"| `orders` | 1 | 1 | 10 | 1 | n/a | n/a | n/a | n/a |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered body misses %q\n%s", want, body)
		}
	}
}

func TestTableStatsHistoryRoundTripsThroughTheDocument(t *testing.T) {
	h := tableStatsHistory{V: 1, Snapshots: []tableStatRecord{
		tsRecord(tsAt(22, 3), map[string][4]int64{"a`b|c": {1, 2, 3, 4}}),
		tsRecord(tsAt(23, 3), map[string][4]int64{"a`b|c": {5, 6, 7, 8}, "z": {0, 0, 0, 0}}),
	}}
	body, err := renderTableStatsDoc(h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTableStatsDoc(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshots) != 2 || got.Snapshots[1].Tables["a`b|c"] != [4]int64{5, 6, 7, 8} || !got.Snapshots[1].At.Equal(tsAt(23, 3)) {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if !strings.Contains(body, "| `a'b\\|c` |") {
		t.Fatalf("table name not escaped in its cell:\n%s", body)
	}
}

func TestParseTableStatsDocRefusesABodyItCannotRead(t *testing.T) {
	if h, err := parseTableStatsDoc("", false); err != nil || len(h.Snapshots) != 0 {
		t.Fatalf("missing document is an empty history, got %+v, %v", h, err)
	}
	if _, err := parseTableStatsDoc("# hand edited, no block\n", true); err == nil {
		t.Fatal("a document without its history block must fail, not reset the history")
	}
	if _, err := parseTableStatsDoc("x\n```json table-stats-history\n{\"v\":1,\"snapshots\":[]}\n", true); err == nil {
		t.Fatal("an unclosed history block must fail")
	}
	if _, err := parseTableStatsDoc("```json table-stats-history\n{\"v\":9,\"snapshots\":[]}\n```\n", true); err == nil {
		t.Fatal("an unknown history version must fail")
	}
}

func TestFitTableStatsDocCapsHistoryAndKeepsTheNewest(t *testing.T) {
	var snaps []tableStatRecord
	for i := 0; i < TableStatsHistoryMax+5; i++ {
		snaps = append(snaps, tsRecord(tsAt(1, 3).AddDate(0, 0, i), map[string][4]int64{"t": {int64(i), 0, 0, 0}}))
	}
	body, err := fitTableStatsDoc(tableStatsHistory{Snapshots: snaps})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTableStatsDoc(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshots) != TableStatsHistoryMax {
		t.Fatalf("kept %d snapshots, want %d", len(got.Snapshots), TableStatsHistoryMax)
	}
	if last := got.Snapshots[len(got.Snapshots)-1]; last.Tables["t"][0] != int64(TableStatsHistoryMax+4) {
		t.Fatalf("newest snapshot not kept: %+v", last.Tables)
	}
}

func TestFitTableStatsDocDropsOldestPastTheBodyBudget(t *testing.T) {
	big := strings.Repeat("x", 200)
	var snaps []tableStatRecord
	for i := 0; i < 20; i++ {
		tables := map[string][4]int64{}
		for j := 0; j < 400; j++ {
			tables[big+strings.Repeat(string(rune('a'+j%26)), 1)+strconv.Itoa(j)] = [4]int64{1, 1, 1, 1}
		}
		snaps = append(snaps, tsRecord(tsAt(1, 3).AddDate(0, 0, i), tables))
	}
	body, err := fitTableStatsDoc(tableStatsHistory{Snapshots: snaps})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > TableStatsBodyBudget {
		t.Fatalf("body %d bytes exceeds the budget %d", len(body), TableStatsBodyBudget)
	}
	got, err := parseTableStatsDoc(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshots) < 2 || len(got.Snapshots) >= 20 {
		t.Fatalf("kept %d snapshots; want some dropped but not all", len(got.Snapshots))
	}
	if !got.Snapshots[len(got.Snapshots)-1].At.Equal(tsAt(1, 3).AddDate(0, 0, 19)) {
		t.Fatal("the newest snapshot must survive the budget trim")
	}
}

func TestQuoteMySQLIdentDoublesBackquotes(t *testing.T) {
	if got := quoteMySQLIdent("yt_write_ops"); got != "`yt_write_ops`" {
		t.Fatalf("plain name quoted as %s", got)
	}
	if got := quoteMySQLIdent("a`b"); got != "`a``b`" {
		t.Fatalf("backquote not doubled: %s", got)
	}
}

// A PostgreSQL store takes no snapshot and touches no pool: it has none.
func TestRunTableStatsIsANoOpOnPostgreSQL(t *testing.T) {
	s := &Store{dialect: postgresDialect}
	run, err := s.RunTableStats(context.Background(), tsAt(24, 4))
	if err != nil || run.Ran {
		t.Fatalf("RunTableStats on PostgreSQL = %+v, %v; want no run and no error", run, err)
	}
}
