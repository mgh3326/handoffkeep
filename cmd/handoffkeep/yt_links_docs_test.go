package main

import (
	"os"
	"strings"
	"testing"
)

// The protocol doc's issue-link section (hk 4640, design §7 and §9 S5) is
// the reference for operators running the canary. These checks fail when the
// section, the lifecycle exception or the authority rule is dropped, or when
// the sentence it replaces comes back.
func TestYTWriteProtocolDocDescribesIssueLinks(t *testing.T) {
	raw, err := os.ReadFile("../../docs/youtrack-write-protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		"## Kind issue_links (hk 3841)",
		"### Canary runbook (hk 4640, hk 4731)",
		"`handoffkeep youtrack links status [--task N] [--limit N]`",
		"`handoffkeep youtrack links plan [--task N] [--project P] [--limit N]`",
		"**Plan the first sweep (mode off).**",
		"This step is production on.",
		"the operator decides from them",
		"`GET /v1/yt/links/status`",
		"Planned kinds (hk 3841, `issue_links`) are the one exception.",
		"`hk:task-fields` and `hk:task-links`",
		"`task_fields` and `task_links` for tasks",
		"`HANDOFFKEEP_YT_LINKS_MODE` (hk 4640)",
		"`link_target_changed`",
		"remote_unknown / recover_exhausted",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("protocol doc lacks %q", want)
		}
	}
	// The sentence the section replaces: issue links no longer stay as the import set.
	if strings.Contains(doc, "Tags and issue links (parent, relations) stay as the import set") {
		t.Error("protocol doc still says issue links stay as the import set")
	}
	// PR #387's wording that binding does not requeue sources is moot (design §7).
	if strings.Contains(doc, "does not requeue") {
		t.Error("protocol doc still carries the PR #387 requeue wording")
	}
	// The runbook is the section's last part, before the authority section,
	// and its steps run plan (mode off) → production on → record (hk 4731).
	start := strings.Index(doc, "## Kind issue_links (hk 3841)")
	runbook := strings.Index(doc, "### Canary runbook (hk 4640, hk 4731)")
	authority := strings.Index(doc, "## Authority (hk 4122)")
	if start < 0 || runbook < start || authority < runbook {
		t.Fatalf("section order: section %d, runbook %d, authority %d", start, runbook, authority)
	}
	plan := strings.Index(doc, "**Plan the first sweep (mode off).**")
	production := strings.Index(doc, "This step is production on.")
	record := strings.Index(doc, "**Record the results**")
	if plan < runbook || production < plan || record < production || record > authority {
		t.Fatalf("runbook step order: plan %d, production %d, record %d", plan, production, record)
	}
}
