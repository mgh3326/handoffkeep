package store

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// NoSelectionText is shown where a job has no recorded selection.
const NoSelectionText = "선택 이유 기록 없음"

// jobRunOthersMax caps the other candidates a JobRun names.
const jobRunOthersMax = 5

// JobRun is the readable Korean summary of one job: what ran, why it was
// chosen, which other candidates lost and how it ended. The console and the
// MCP task_get serve the same fields, so a page and an agent read one text.
type JobRun struct {
	What     string   `json:"what"`
	Why      string   `json:"why"`
	Override string   `json:"override,omitempty"`
	Others   []string `json:"others"`
	Result   string   `json:"result"`
	// Recorded is false when the job has no selection record (Why then
	// says NoSelectionText).
	Recorded bool `json:"selection_recorded"`
}

// Selection keys hk renders (panewire owns the rest and hk keeps it):
//
//	chosen.why, chosen.grade (model grade), chosen.tier, chosen.model_id,
//	chosen.seat / chosen.canon, chosen.projected_pct, chosen.waste_urgency,
//	chosen.reset_in_s (seconds until the quota resets),
//	candidates[].{seat|canon|model_id, outcome},
//	policy.urgency_source,
//	override_reason (or override.reason).
//
// A key that is missing or has another type is skipped, never an error.

var selectionTaskGradeRE = regexp.MustCompile(`for task (\S+)`)

var candidateOutcomeLabel = map[string]string{
	"below_choice":               "선택된 후보보다 순위가 낮음",
	"admitted":                   "통과했지만 선택되지 않음",
	"dropped:role_filtered":      "역할 조건에 맞지 않아 제외",
	"dropped:grade_insufficient": "모델 등급이 작업 등급보다 낮아 제외",
}

var jobStateLabel = map[string]string{
	"claimed": "인수됨", "spawned": "실행 중", "completed": "완료", "escalated": "에스컬레이션", "lost": "유실",
}

var selectionTierLabel = map[string]string{"waste": "waste(낭비 위험)"}

// Explain builds the readable summary of j.
func (j Job) Explain() JobRun {
	var sel map[string]any
	if len(j.Selection) > 0 {
		_ = json.Unmarshal(j.Selection, &sel)
	}
	run := JobRun{Others: []string{}, Result: jobResultText(j)}
	chosen, _ := sel["chosen"].(map[string]any)
	run.What = jobWhatText(j, chosen)
	run.Why = NoSelectionText
	if chosen != nil {
		if why := jobWhyText(chosen, sel); why != "" {
			run.Why, run.Recorded = why, true
		}
	}
	run.Override = jobOverrideText(sel)
	if run.Override != "" {
		run.Recorded = true
	}
	run.Others = jobOthersText(sel, chosen)
	if len(run.Others) > 0 {
		run.Recorded = true
	}
	return run
}

func jobWhatText(j Job, chosen map[string]any) string {
	model := j.Model
	if model == "" {
		model = selStr(chosen, "model_id")
	}
	host := j.Host
	var parts []string
	add := func(label, value string) {
		if value != "" {
			parts = append(parts, label+" "+value)
		}
	}
	add("모델", model)
	if j.Harness != nil {
		add("하네스", *j.Harness)
	}
	if j.Effort != nil {
		add("effort", *j.Effort)
	}
	add("프로필", j.Profile)
	add("호스트", host)
	add("역할", j.Role)
	if len(parts) == 0 {
		return "기록 없음"
	}
	return strings.Join(parts, " · ")
}

// jobWhyText turns chosen into one sentence. Structured fields win; the why
// text only supplies the task grade, and is shown verbatim when no structured
// field can be rendered.
func jobWhyText(chosen, sel map[string]any) string {
	why := selStr(chosen, "why")
	var head string
	taskGrade := ""
	if m := selectionTaskGradeRE.FindStringSubmatch(why); m != nil {
		taskGrade = m[1]
	}
	modelGrade := selStr(chosen, "grade")
	switch {
	case taskGrade != "" && modelGrade != "":
		head = fmt.Sprintf("작업 등급 %s에 모델 등급 %s를 골랐습니다", taskGrade, modelGrade)
	case modelGrade != "":
		head = fmt.Sprintf("모델 등급 %s를 골랐습니다", modelGrade)
	}
	var facts []string
	if tier := selStr(chosen, "tier"); tier != "" {
		label, ok := selectionTierLabel[tier]
		if !ok {
			label = tier
		}
		facts = append(facts, "쿼터 tier "+label)
	}
	if pct, ok := selNum(chosen, "projected_pct"); ok {
		facts = append(facts, fmt.Sprintf("예상 사용률 %d%%", int(math.Round(pct))))
	}
	if secs, ok := selNum(chosen, "reset_in_s"); ok && secs >= 0 {
		facts = append(facts, "리셋까지 "+koDuration(time.Duration(secs*float64(time.Second))))
	}
	if urgency, ok := selNum(chosen, "waste_urgency"); ok {
		text := fmt.Sprintf("긴급도 %.2f", urgency)
		if source := selStr(sel["policy"], "urgency_source"); source != "" {
			text += " (출처 " + source + ")"
		}
		facts = append(facts, text)
	}
	switch {
	case head != "" && len(facts) > 0:
		return head + ": " + strings.Join(facts, ", ") + "."
	case head != "":
		return head + "."
	case len(facts) > 0:
		return strings.Join(facts, ", ") + "."
	}
	return clipRunes(why, 300)
}

func jobOverrideText(sel map[string]any) string {
	if reason := selStr(sel, "override_reason"); reason != "" {
		return clipRunes(reason, 200)
	}
	return clipRunes(selStr(sel["override"], "reason"), 200)
}

func candidateName(c any) string {
	for _, key := range []string{"seat", "canon", "model_id"} {
		if name := selStr(c, key); name != "" {
			return name
		}
	}
	return ""
}

// jobOthersText names the candidates that did not run, closest runners-up
// first (below_choice), then the dropped ones, in recorded order.
func jobOthersText(sel, chosen map[string]any) []string {
	candidates, _ := sel["candidates"].([]any)
	chosenName := candidateName(chosen)
	var near, dropped []string
	for _, c := range candidates {
		name, outcome := candidateName(c), selStr(c, "outcome")
		if name == "" || outcome == "" || (name == chosenName && outcome == "admitted") {
			continue
		}
		label, known := candidateOutcomeLabel[outcome]
		if !known {
			label = clipRunes(outcome, 80)
		}
		line := clipRunes(name, 80) + " — " + label
		if outcome == "below_choice" || outcome == "admitted" {
			near = append(near, line)
		} else {
			dropped = append(dropped, line)
		}
	}
	all := append(near, dropped...)
	if len(all) > jobRunOthersMax {
		rest := len(all) - jobRunOthersMax
		all = append(all[:jobRunOthersMax:jobRunOthersMax], fmt.Sprintf("외 %d개", rest))
	}
	if all == nil {
		return []string{}
	}
	return all
}

func jobResultText(j Job) string {
	state := jobStateLabel[j.State]
	if state == "" {
		state = j.State
	}
	parts := []string{"상태 " + state}
	if j.EndReason != nil && *j.EndReason != "" {
		parts = append(parts, "종료 사유 "+*j.EndReason)
	}
	if j.SpawnedAt != nil && j.EndedAt != nil && !j.EndedAt.Before(*j.SpawnedAt) {
		parts = append(parts, "소요 "+koDuration(j.EndedAt.Sub(*j.SpawnedAt)))
	}
	if j.Usage != nil {
		var tokens []string
		for _, t := range []struct {
			label string
			n     *int64
		}{{"입력", j.Usage.InputTokens}, {"출력", j.Usage.OutputTokens}, {"추론", j.Usage.ReasoningTokens}, {"캐시 읽기", j.Usage.CacheReadTokens}} {
			if t.n != nil {
				tokens = append(tokens, t.label+" "+groupDigits(*t.n))
			}
		}
		if len(tokens) > 0 {
			parts = append(parts, "토큰 "+strings.Join(tokens, " / "))
		}
	}
	if j.DiffStat != nil {
		parts = append(parts, fmt.Sprintf("diff %d files +%d −%d", j.DiffStat.Files, j.DiffStat.Insertions, j.DiffStat.Deletions))
	}
	return strings.Join(parts, " · ")
}

// koDuration renders d at minute precision: "1시간 5분", "12분", "1분 미만".
func koDuration(d time.Duration) string {
	mins := int(d / time.Minute)
	switch {
	case mins < 1:
		return "1분 미만"
	case mins < 60:
		return strconv.Itoa(mins) + "분"
	case mins%60 == 0:
		return strconv.Itoa(mins/60) + "시간"
	}
	return fmt.Sprintf("%d시간 %d분", mins/60, mins%60)
}

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func selStr(v any, key string) string {
	m, _ := v.(map[string]any)
	s, _ := m[key].(string)
	return s
}

func selNum(v any, key string) (float64, bool) {
	m, _ := v.(map[string]any)
	f, ok := m[key].(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
