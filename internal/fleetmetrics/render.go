package fleetmetrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

func fmtDist(d *Dist, unit string) string {
	if d == nil {
		return "not observed (n=0)"
	}
	return fmt.Sprintf("p50 %.2f%s · p90 %.2f%s · max %.2f%s (n=%d)", d.P50, unit, d.P90, unit, d.Max, unit, d.N)
}

func fmtShare(num, den int) string {
	if den == 0 {
		return "not observed (denominator 0)"
	}
	return fmt.Sprintf("%d/%d = %.1f%%", num, den, 100*float64(num)/float64(den))
}

func fmtCoverage(c Coverage) string {
	pct := "n/a"
	if c.Total > 0 {
		pct = fmt.Sprintf("%.1f%%", 100*float64(c.Linked)/float64(c.Total))
	}
	line := fmt.Sprintf("COVERAGE %s: %d/%d (%s)", c.What, c.Linked, c.Total, pct)
	if len(c.Detail) > 0 {
		keys := make([]string, 0, len(c.Detail))
		for k := range c.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, c.Detail[k]))
		}
		line += " — " + strings.Join(parts, " · ")
	}
	if c.Total > 0 && float64(c.Linked)/float64(c.Total) < 0.8 {
		line += " ⚠ below 80%: the value covers only the linked subset"
	}
	return line
}

func fmtCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none recorded"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

func fmtHours(h *float64) string {
	if h == nil {
		return "none"
	}
	return fmt.Sprintf("%.2fh", *h)
}

// RenderMarkdown writes the report in the daily hk report-doc format.
func RenderMarkdown(w io.Writer, r Report) {
	kst := time.FixedZone("KST", 9*3600)
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	cov := func(cs []Coverage) {
		for _, c := range cs {
			p("- %s", fmtCoverage(c))
		}
	}
	p("# fleet-metrics %s → %s (KST)", r.Since.In(kst).Format("2006-01-02 15:04"), r.Until.In(kst).Format("2006-01-02 15:04"))
	p("")
	p("collected %s · definitions: hk:doc advice/2026-09-24/fleet-structure-astra Q6 · unknown values are never 0", r.CollectedAt.In(kst).Format(time.RFC3339))
	p("")
	p("## 1. order → usable result (hours)")
	lt := r.LeadTime
	p("- deploy group (usable = first successful deploy record that includes the merge): %s", fmtDist(lt.Deploy, "h"))
	p("- merge-only group (repo has no deploy-record stream; usable = merge): %s", fmtDist(lt.MergeOnly, "h"))
	for _, k := range []string{"implement", "verify_fix", "decision_wait", "install"} {
		p("- phase %s: %s", k, fmtDist(lt.Phases[k], "h"))
	}
	p("  - decision_wait 0h is an observed zero (the task never entered needs_decision), not a missing value")
	p("- completed (merged in window): %d", lt.Completed)
	p("- open work: %d ordered, age %s; %d open without a claim event", lt.OpenCount, fmtDist(lt.OpenAge, "h"), lt.OpenUnordered)
	cov(lt.Coverage)
	p("")
	p("## 2. verify rounds per contract lineage")
	rd := r.Rounds
	p("- rounds per lineage (count of → verifying): %s", fmtDist(rd.PerLineage, ""))
	p("- share over 3 rounds: %s", fmtShare(rd.OverThree, rd.Lineages))
	p("- fix rounds (verifying → in_progress): %d · cause: %s", rd.FixRounds, fmtCounts(rd.Cause))
	p("- scopefuel impl reps rounds per lineage: %s · agree with hk %d · disagree %d", fmtDist(rd.RepsRounds, ""), rd.RepsAgree, rd.RepsDisagree)
	cov(rd.Coverage)
	p("")
	p("## 3. empty task-slot minutes while ready work existed")
	sl := r.EmptySlots
	p("- share (upper bound on waste: eligibility is not recorded): %s … %s", fmtShare(sl.EmptyWithReady, sl.SlotMinutes), fmtShare(sl.EmptyWithReadyMax, sl.SlotMinutes))
	p("  - the range is [every unobserved unit held its slot … every unobserved unit was gone]; equal ends mean nothing was unobserved")
	p("- slot-minutes %d = working %d + held-idle %d (live builder not WORKING: waiting on tester, CI, answer or merge) + empty %d + unobserved %d · overflow beyond capacity %d", sl.SlotMinutes, sl.Working, sl.HeldIdle, sl.Empty, sl.Unobserved, sl.Overflow)
	p("- empty minutes by reason: %s", fmtCounts(sl.ByReason))
	for _, m := range sl.PerMachine {
		p("- %s (%d slots): %d slot-min · working %d · held-idle %d · empty %d · unobserved %d · empty-with-ready %d…%d", m.Machine, m.Slots, m.SlotMinutes, m.Working, m.HeldIdle, m.Empty, m.Unobserved, m.EmptyWithReady, m.EmptyWithReadyMax)
	}
	cov(sl.Coverage)
	p("")
	p("## 4. decision-request dwell (hours)")
	dc := r.Decisions
	p("- requests in window: %d (%s)", dc.Requests, fmtCounts(dc.ByKind))
	p("- request → answer: %s", fmtDist(dc.ToAnswer, "h"))
	kinds := make([]string, 0, len(dc.PerKind))
	for k := range dc.PerKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		p("  - %s: %s", k, fmtDist(dc.PerKind[k], "h"))
	}
	p("- answer → delivery: %s", fmtDist(dc.AnswerToDeliver, "h"))
	p("- answer → director consumption: %s", fmtDist(dc.AnswerToConsume, "h"))
	p("- answered by: %s · auto-default answers: n/a (no auto-default writer exists)", fmtCounts(dc.AnsweredBy))
	p("- open now: %d · oldest %s %s", dc.Open, fmtHours(dc.OldestOpen), dc.OldestOpenRef)
	cov(dc.Coverage)
	p("")
	p("## 5. finished on the normal path (no operational repair)")
	np := r.NormalPath
	p("- share: %s (terminal %d, of which dropped %d)", fmtShare(np.NormalPath, np.Classified), np.Terminal, np.Dropped)
	p("- repaired: %d · by class: %s", np.Repaired, fmtCounts(np.ByRepair))
	p("- open tasks with a repair signal: %d · oldest %s", np.OpenRepaired, fmtHours(np.OpenOldest))
	cov(np.Coverage)
	p("")
	p("## sources")
	for _, s := range r.Sources {
		p("- %s", s)
	}
	for _, n := range r.Notes {
		p("- note: %s", n)
	}
}
