package main

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestSeriesParent(t *testing.T) {
	cases := []struct {
		title string
		want  int64
		ok    bool
	}{
		{"[2698 S3] hk decision inbox", 2698, true},
		{"[2698 lead] hk tracker: run slices", 2698, true},
		{"[2698 later] follow-ups", 2698, true},
		{"[2698 S0b] docs: as-built", 2698, true},
		{"prefix [2698 S3] not at start", 0, false},
		{"[abc S3] not numeric", 0, false},
		{"[] empty", 0, false},
		{"no bracket at all", 0, false},
	}
	for _, c := range cases {
		got, ok := seriesParent(c.title)
		if ok != c.ok || got != c.want {
			t.Errorf("seriesParent(%q) = %d,%v; want %d,%v", c.title, got, ok, c.want, c.ok)
		}
	}
}

func TestRelatedTarget(t *testing.T) {
	cases := []struct {
		title string
		want  int64
		ok    bool
	}{
		{"[follow-up of hk 1276 / PR 62, operator A 10-09] handoffkeep remote client", 1276, true},
		{"wrk sentinel follow-up to hk 1959 (PR 249): the late=1 log field", 1959, true},
		{"O3 follow-up of hk 2040 (dr-2040-1 A): a successor", 2040, true},
		{"R-25 follow-up from hk 2041 (grade B, T1): upload_report_document", 2041, true},
		{"scopefuel commandcode probe follow-ups from hk 2182 CodeRabbit Minors", 2182, true},
		{"Web QA harness follow-ups from hk 1396 round-3 tester (PR #22)", 1396, true},
		{"runtug: spawn on a hub-only host — respawn of hk 2645 after codex-sol", 2645, true},
		{"mergewake review: close the reviewer pane — fresh redo of hk 2736 from main", 2736, true},
		{"panewire: a --parent-job respawn of a terminal parent job — redo of hk 2799 after codex ESC", 2799, true},
		{"panewire batch follow-ups (grade C, T1; b953 10-28 per director-1, from hk 2208 PR 110 thread checks; hk 2208 comment 8127): (1) scripts", 2208, true},
		// "hk N follow-up" names the origin before the keyword; the trailing
		// "from hk M" is the tester-finding source, not the origin.
		{"hk 2152 follow-up (grade A, T2, fast pool; director-1 10-27, from hk 1926 r2 tester finding S-MAIN-2): the artifact prune", 0, false},
		// No of/to/from before the hk reference.
		{"Follow-up (hk 1656 r4 B2): slot-fit 14 lp corpus", 0, false},
		{"device QA 10-21 P0 follow-up, hk 1800 measurement", 0, false},
		// The hk reference is an operator decision, not the followed-up task.
		{"[#890 follow-up T2 · operator hk 1225 A] approval inbox", 0, false},
		// A bundle of follow-ups from many tasks names no single origin.
		{"Tester follow-ups bundle (non-blocking SHOULD/NICE): #42 3 SHOULD (hk 1433 verdict-r3), #35 (hk 1431)", 0, false},
		// Not a follow-up/redo/respawn title.
		{"r1026effdown B-arm twin of hk 2011 (haiku@medium side)", 0, false},
		{"plain task with no relation words", 0, false},
	}
	for _, c := range cases {
		got, ok := relatedTarget(c.title)
		if ok != c.ok || got != c.want {
			t.Errorf("relatedTarget(%q) = %d,%v; want %d,%v", c.title, got, ok, c.want, c.ok)
		}
	}
}

func TestHierarchyBackfillPlan(t *testing.T) {
	parent := int64(2698)
	tasks := []store.Task{
		{ID: 2698, Title: "hk tracker: internalize Linear"},
		{ID: 2770, Title: "[2698 S1] hk task hierarchy", ParentID: nil},
		{ID: 2994, Title: "[2698 S0b] docs: as-built", ParentID: &parent},
		{ID: 1307, Title: "[follow-up of hk 1276 / PR 62] remote client"},
		{ID: 1276, Title: "handoffkeep remote client"},
		{ID: 2169, Title: "hk 2152 follow-up (from hk 1926 r2 tester finding)"},
		{ID: 9999, Title: "[1234 S1] parent not in set"},
	}
	parents, relations := hierarchyBackfillPlan(tasks)
	if len(parents) != 1 || parents[0].TaskID != 2770 || parents[0].Target != 2698 {
		t.Fatalf("parents = %+v; want one parent op 2770->2698", parents)
	}
	if len(relations) != 1 || relations[0].TaskID != 1307 || relations[0].Target != 1276 {
		t.Fatalf("relations = %+v; want one related op 1307->1276", relations)
	}
}
