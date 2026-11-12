package youtrack

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The issue text is a port of the Python mirror that wrote these issues
// before the Go sync (yt_mirror.py, hk 3365). Each function keeps the name
// and the order of the lines it came from, so the two can be read side by
// side. Everything here is pure: the caller reads the store and passes the
// values in.

const (
	descriptionTimeLayout = "2006-01-02 15:04"
	commentTextLimit      = 60000
	summaryLimit          = 250
)

var (
	// noiseRE marks a task comment the machinery wrote (spawn logs, lane
	// notices). Those are not mirrored; the mirror only carries human text.
	noiseRE = regexp.MustCompile(`(?i)^\s*(spawned request=|spawn failed|panewire job\.(completed|escalated|lost)|blocks:|Expected:|design (check|review)|Scope (add|change|extended)|Rescoped|direct-merge|Restart decision|Second attempt|Correction|Field contract|ESC answer|\[mergewake\]|mergewake |ledger|landing|request=)`)
	// suffixRE is the "(grade B, T2)" or "(T2)" marker at the end of a title.
	suffixRE   = regexp.MustCompile(`\s*\((grade (` + store.TaskGradePattern() + `), ?T[0-3]|T[0-3])\)\s*$`)
	hkPrefixRE = regexp.MustCompile(`^\[hk \d+\]\s*`)
	workerRE   = regexp.MustCompile(`host=(\S+) profile=(\S+)`)
)

// gateKO names the merge-gate reasons the console and the issue show.
var gateKO = map[string]string{
	"conflict": "충돌", "review_missing": "리뷰 대기", "review_stalled": "리뷰 멈춤", "findings": "리뷰 지적 미해결",
	"unresolved_findings": "리뷰 지적 미해결", "checks": "CI 실패", "checks_failed": "CI 실패", "required_checks": "CI 대기",
	"base_stale": "기준 브랜치 뒤처짐", "draft": "초안", "deploy_approval": "배포 승인 필요", "merge_overdue": "머지 지연",
}

// briefSection is one section of the brief the issue quotes.
type briefSection struct {
	label, stop string
	limit       int
	re, stopRE  *regexp.Regexp
}

var briefSections = []briefSection{
	{label: `(?m)^Why:\s*`, stop: `(?m)^(Current|Desired|Key interfaces|Out of scope|##)`, limit: 900},
	{label: `(?m)^Desired:\s*`, stop: `(?m)^(Out of scope|Key interfaces|##)`, limit: 1800},
	{label: `(?m)^## Acceptance[^\n]*\n`, stop: `(?m)^(Copy this section|##)`, limit: 1500},
}

func init() {
	for i := range briefSections {
		briefSections[i].re = regexp.MustCompile(briefSections[i].label)
		briefSections[i].stopRE = regexp.MustCompile(briefSections[i].stop)
	}
}

// truncateRunes cuts s to n characters, as Python slices a str.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// cleanTitle is the YouTrack summary of a task: the display title without the
// hk number prefix and the grade marker, cut to the summary limit. The title
// passed in is already the translation when one exists.
func cleanTitle(title string) string {
	title = hkPrefixRE.ReplaceAllString(title, "")
	title = strings.TrimSpace(suffixRE.ReplaceAllString(title, ""))
	return truncateRunes(title, summaryLimit)
}

// gradeFromTitle and tierFromTitle read the marker the title carries. Both
// come back empty when the title has no "(grade X, Tn)" marker.
func gradeFromTitle(title string) (grade, tier string) { return ytwrite.TitleGradeTier(title) }

// humanComment reports whether a task comment is worth mirroring.
func humanComment(body string) bool {
	return !noiseRE.MatchString(body)
}

// commentText is the body of the YouTrack comment for one hk comment: who
// wrote it, when, the text, and the marker that names the hk comment. The
// marker lets a run that lost the create response find the comment and link
// it instead of posting it again.
func commentText(c store.TaskComment) string {
	who := "운영자"
	if !strings.HasPrefix(c.Author, "operator") {
		who = fmt.Sprintf("에이전트 메모 (%s)", c.Author)
	}
	marker := commentMarker(c.ID)
	text := fmt.Sprintf("**%s** · %s\n\n%s", who, c.CreatedAt.UTC().Format(descriptionTimeLayout), c.Body)
	return truncateRunes(text, commentTextLimit-len(marker)-len("\n\n")) + "\n\n" + marker
}

// commentMarker is the HTML comment that ends a mirrored comment. Its id is the
// hk comment id, which is what the sync matches on.
func commentMarker(hkCommentID int64) string {
	return fmt.Sprintf("<!-- hk-comment:%d -->", hkCommentID)
}

// workerLine names the host and profile of the newest comment that carries
// them ("host=... profile=..."), or "" when none does.
func workerLine(comments []store.TaskComment) string {
	sorted := append([]store.TaskComment(nil), comments...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		}
		return sorted[i].ID > sorted[j].ID
	})
	for _, c := range sorted {
		if m := workerRE.FindStringSubmatch(c.Body); m != nil {
			return m[1] + " · " + m[2]
		}
	}
	return ""
}

// quotedBriefSections returns the Why, Desired and Acceptance sections of a
// brief, each cut to its limit. A section the brief lacks is left out, so
// the positions shift the way the Python mirror's did.
func quotedBriefSections(body string) []string {
	var out []string
	for _, s := range briefSections {
		loc := s.re.FindStringIndex(body)
		if loc == nil {
			continue
		}
		rest := body[loc[1]:]
		chunk := rest
		if end := s.stopRE.FindStringIndex(rest); end != nil {
			chunk = rest[:end[0]]
		}
		chunk = strings.TrimSpace(chunk)
		if utf8.RuneCountInString(chunk) > s.limit {
			chunk = truncateRunes(chunk, s.limit)
			if i := strings.LastIndex(chunk, "\n"); i >= 0 {
				chunk = chunk[:i]
			}
			chunk += "\n…"
		}
		out = append(out, chunk)
	}
	return out
}

// prView is the part of a change the issue shows.
type prView struct {
	Repo     string
	Number   int
	URL      string
	Title    string
	State    string
	MergedAt *time.Time
	Head     string
	Gate     string
	Reasons  []string
	Fails    []string
	Findings int
	High     int
}

// prStateKO names a change state for the issue.
func prStateKO(state string) string {
	switch state {
	case "open":
		return "열림"
	case "closed":
		return "닫힘"
	case "merged":
		return "머지됨"
	}
	return state
}

// issueText is everything the issue text reads from hk.
type issueText struct {
	Task     store.Task
	Console  string
	BriefKey string
	Brief    string
	PR       *prView
	Comments []store.TaskComment
	Events   []store.TaskEvent
}

// descriptionFor renders the issue text, in the order the Python mirror wrote it.
func descriptionFor(b issueText) string {
	t := b.Task
	consoleURL := fmt.Sprintf("%s/ui/tasks/%d", strings.TrimRight(b.Console, "/"), t.ID)
	lines := []string{fmt.Sprintf("[hk 콘솔에서 열기](%s)", consoleURL)}

	// The brief block needs a brief with at least one section; a brief that
	// quotes nothing adds no line at all.
	if secs := quotedBriefSections(b.Brief); len(secs) > 0 {
		lines = append(lines, "", "## 하는 일", secs[0])
		if len(secs) > 1 {
			lines = append(lines, "", "## 바라는 결과", secs[1])
		}
		if len(secs) > 2 {
			lines = append(lines, "", "## 완료 조건", secs[2])
		}
		lines = append(lines, "", fmt.Sprintf("브리프: `%s`", b.BriefKey))
	}

	if pr := b.PR; pr != nil {
		lines = append(lines, "", "## PR",
			fmt.Sprintf("- [%s#%d](%s) %s", pr.Repo, pr.Number, pr.URL, pr.Title))
		status := fmt.Sprintf("- 상태: %s · head `%s`", prStateKO(pr.State), pr.Head)
		if pr.MergedAt != nil {
			status += " · 머지 " + pr.MergedAt.UTC().Format(descriptionTimeLayout)
		}
		lines = append(lines, status)
		if pr.Gate != "" {
			gate := "- 머지 판정: " + pr.Gate
			if len(pr.Reasons) > 0 {
				names := make([]string, 0, len(pr.Reasons))
				for _, r := range pr.Reasons {
					names = append(names, reasonKO(r))
				}
				gate += " (" + strings.Join(names, ", ") + ")"
			}
			lines = append(lines, gate)
		}
		if len(pr.Fails) > 0 {
			lines = append(lines, "- 실패한 CI: "+strings.Join(pr.Fails, ", "))
		}
		if pr.Findings > 0 {
			lines = append(lines, fmt.Sprintf("- 열린 리뷰 지적: %d건 (높음 이상 %d건)", pr.Findings, pr.High))
		}
	}

	lines = append(lines, "", "## 진행")
	if t.Refs.JobID != "" {
		item := "- 워커: `" + t.Refs.JobID + "`"
		if w := workerLine(b.Comments); w != "" {
			item += " (" + w + ")"
		}
		lines = append(lines, item)
	}
	for _, row := range []struct {
		at    *time.Time
		label string
	}{{t.StartedAt, "시작"}, {&t.LastActivityAt, "최근 활동"}, {t.CompletedAt, "종료"}} {
		if row.at != nil && !row.at.IsZero() {
			lines = append(lines, fmt.Sprintf("- %s: %s", row.label, row.at.UTC().Format(descriptionTimeLayout)))
		}
	}
	if t.Expected != nil && *t.Expected != "" {
		lines = append(lines, "- 예상 시간: "+*t.Expected)
	}
	if c := t.Refs.Closure; c != nil && c.Evidence != "" {
		lines = append(lines, fmt.Sprintf("- 종료 근거: %s %s", c.Type, c.Evidence))
	}

	var changes []store.TaskEvent
	for _, e := range b.Events {
		if e.From != e.To {
			changes = append(changes, e)
		}
	}
	if len(changes) > 6 {
		changes = changes[len(changes)-6:]
	}
	if len(changes) > 0 {
		lines = append(lines, "", "## 상태 기록")
		for _, e := range changes {
			lines = append(lines, fmt.Sprintf("- %s %s → %s (%s)",
				e.At.UTC().Format(descriptionTimeLayout), StateLabel(e.From), StateLabel(e.To), e.By))
		}
	}

	lines = append(lines, "", fmt.Sprintf("<sub>원문 제목: %s</sub>", t.Title), "",
		"_hk에서 옮긴 읽기 전용 내용입니다. 상태·우선순위는 hk 기준입니다._")
	return strings.Join(lines, "\n")
}

// reasonKO names a merge-gate reason; an unknown one is shown as it is.
func reasonKO(reason string) string {
	if ko, ok := gateKO[reason]; ok {
		return ko
	}
	return reason
}

// openFinding reports whether a finding on head still needs an answer.
func openFinding(status string) bool {
	return status == "open" || status == "unresolved"
}
