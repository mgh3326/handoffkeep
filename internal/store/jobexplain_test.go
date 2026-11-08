package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// panewireSample is the chosen entry pinned in the hk 3516 brief, with a
// candidate list of each outcome kind.
const panewireSample = `{
 "chosen":{"account_key":"codex:fc823b1db7f49cd4","canon":"codex-terra61","effort":"high","gate":"default","grade":"A+","host":"mac-dev","model_id":"gpt-6.1-terra","outcome_code":"ADMITTED","pool":"codex","projected_pct":75.53509510730082,"seat":"codex-terra61","tier":"waste","waste_risk":true,"waste_urgency":0.30954625083299686,"why":"waste_risk tier=waste grade=A+ for task B projected=76% waste_urgency=0.31"},
 "candidates":[
  {"seat":"codex-terra61","outcome":"admitted"},
  {"seat":"claude-opus","outcome":"dropped:role_filtered"},
  {"seat":"claude-sonnet","outcome":"below_choice"},
  {"seat":"codex-mini","outcome":"dropped:grade_insufficient"},
  {"seat":"local-x","outcome":"dropped:quota_exhausted"}
 ],
 "policy":{"urgency_source":"quota-forecast"},
 "future_key":{"kept":true}
}`

func TestExplainSample(t *testing.T) {
	end := time.Date(2026, 11, 8, 3, 5, 0, 0, time.UTC)
	start := end.Add(-65 * time.Minute)
	in, out := int64(1200), int64(340)
	harness, effort, reason := "codex", "high", "ready"
	j := Job{
		JobID: "j", State: "completed", Role: "builder", Profile: "terra@high", Host: "mac-dev",
		Model: "gpt-6.1-terra", Harness: &harness, Effort: &effort, EndReason: &reason,
		SpawnedAt: &start, EndedAt: &end, Selection: json.RawMessage(panewireSample),
		Usage:    &JobUsage{InputTokens: &in, OutputTokens: &out},
		DiffStat: &JobDiffStat{Files: 3, Insertions: 120, Deletions: 8},
	}
	run := j.Explain()
	if want := "모델 gpt-6.1-terra · 하네스 codex · effort high · 프로필 terra@high · 호스트 mac-dev · 역할 builder"; run.What != want {
		t.Fatalf("what: %q", run.What)
	}
	want := "작업 등급 B에 모델 등급 A+를 골랐습니다: 쿼터 tier waste(낭비 위험), 예상 사용률 76%, 긴급도 0.31 (출처 quota-forecast)."
	if run.Why != want {
		t.Fatalf("why: %q", run.Why)
	}
	wantOthers := []string{
		"claude-sonnet — 선택된 후보보다 순위가 낮음",
		"claude-opus — 역할 조건에 맞지 않아 제외",
		"codex-mini — 모델 등급이 작업 등급보다 낮아 제외",
		"local-x — dropped:quota_exhausted",
	}
	if strings.Join(run.Others, "|") != strings.Join(wantOthers, "|") {
		t.Fatalf("others: %q", run.Others)
	}
	wantResult := "상태 완료 · 종료 사유 ready · 소요 1시간 5분 · 토큰 입력 1,200 / 출력 340 · diff 3 files +120 −8"
	if run.Result != wantResult || !run.Recorded {
		t.Fatalf("result: %q recorded=%v", run.Result, run.Recorded)
	}
}

func TestExplainWithoutSelection(t *testing.T) {
	run := Job{JobID: "j", State: "spawned", Role: "builder", Host: "h", Model: "m"}.Explain()
	if run.Why != NoSelectionText || run.Recorded || len(run.Others) != 0 || run.Others == nil {
		t.Fatalf("%+v", run)
	}
	if run.What != "모델 m · 호스트 h · 역할 builder" || run.Result != "상태 실행 중" {
		t.Fatalf("%+v", run)
	}
	// An empty object is no record.
	run = Job{JobID: "j", Selection: json.RawMessage(`{}`)}.Explain()
	if run.Why != NoSelectionText || run.Recorded {
		t.Fatalf("empty: %+v", run)
	}
}

// A stored selection is a record even when hk renders none of its keys.
func TestExplainUnrenderedSelectionIsRecorded(t *testing.T) {
	for _, raw := range []string{`{"future_key":1}`, `{"chosen":{"model_id":"m"}}`, `{"chosen":"x","candidates":3,"policy":[]}`, `{"chosen":{"grade":4,"projected_pct":"high"}}`} {
		run := Job{JobID: "j", Selection: json.RawMessage(raw)}.Explain()
		if run.Why != UnrenderedSelectionText || !run.Recorded {
			t.Fatalf("%s: %+v", raw, run)
		}
	}
}

func TestExplainOverrideAndFallbackWhy(t *testing.T) {
	run := Job{Selection: json.RawMessage(`{"chosen":{"why":"operator pinned model"},"override_reason":"-m gpt-x"}`)}.Explain()
	if run.Why != "operator pinned model" || run.Override != "-m gpt-x" || !run.Recorded {
		t.Fatalf("%+v", run)
	}
	run = Job{Selection: json.RawMessage(`{"override":{"reason":"--host mac-dev"}}`)}.Explain()
	if run.Override != "--host mac-dev" || run.Why != UnrenderedSelectionText || !run.Recorded {
		t.Fatalf("%+v", run)
	}
}

func TestExplainOthersCap(t *testing.T) {
	var cands []string
	for i := 0; i < 8; i++ {
		cands = append(cands, `{"seat":"s`+string(rune('a'+i))+`","outcome":"below_choice"}`)
	}
	run := Job{Selection: json.RawMessage(`{"candidates":[` + strings.Join(cands, ",") + `]}`)}.Explain()
	if len(run.Others) != jobRunOthersMax+1 || run.Others[jobRunOthersMax] != "외 3개" {
		t.Fatalf("%q", run.Others)
	}
}

func TestGroupDigitsAndDuration(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := groupDigits(in); got != want {
			t.Fatalf("%d: %s", in, got)
		}
	}
	for in, want := range map[time.Duration]string{30 * time.Second: "1분 미만", 12 * time.Minute: "12분", time.Hour: "1시간", 3*time.Hour + 12*time.Minute: "3시간 12분"} {
		if got := koDuration(in); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
}

func TestCheckJobSelection(t *testing.T) {
	if err := checkJobSelection(json.RawMessage(panewireSample)); err != nil {
		t.Fatalf("sample refused: %v", err)
	}
	pad := `{"pad":"` + strings.Repeat("x", JobSelectionMaxBytes) + `"}`
	for name, raw := range map[string]string{
		"null": `null`, "array": `[]`, "string": `"x"`, "number": `1`, "trailing": `{} {}`, "malformed": `{`,
		"too large": pad, "NUL": `{"a":"\u0000"}`, "secret": `{"a":"AKIA` + strings.Repeat("A", 16) + `"}`,
	} {
		if err := checkJobSelection(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	exact := `{"pad":"` + strings.Repeat("x", JobSelectionMaxBytes-len(`{"pad":""}`)) + `"}`
	if len(exact) != JobSelectionMaxBytes {
		t.Fatalf("fixture is %d bytes", len(exact))
	}
	if err := checkJobSelection(json.RawMessage(exact)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
}
