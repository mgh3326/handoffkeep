package store

import (
	"encoding/json"
	"strings"
	"testing"
)

const deploySample = `Approve deployment {"identity":{"environment_known":true,"config":"914e2e108bed9b930c8824032505202a7d3d22925ac66c33f4e34aa7c550f391","payload":"d7fb23860cc0792c61029238a239e9f2a2e433085c1def188202af7aa536538d"},"host":"mbp","repo":"mgh3326/handoffkeep","previous":"unknown","release":"d4b91c0693df53895052b7a502eedb05c331c7d6","paths":[],"reasons":["host inventory approval missing or changed","missing or invalid previous deployment receipt"]}`

func TestMergewakeDeployTemplate(t *testing.T) {
	request := DecisionRequest{RequestedBy: "mergewake-design", Question: deploySample}
	options := DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}
	before, _ := json.Marshal(options)
	out, ok := RenderDecisionTemplate(request, &options)
	if !ok {
		t.Fatal("sample did not match")
	}
	for _, want := range []string{"handoffkeep d4b91c0", "mbp", "대상 호스트", "저장소", "릴리스", "이전 배포", "승인이 필요한 경로", "사유", "이전 배포 기록이 없음", "hk 규칙"} {
		if !strings.Contains(out.Question, want) {
			t.Errorf("question missing %q: %s", want, out.Question)
		}
	}
	if !out.Options.Options[0].Recommended || out.Options.Options[1].Recommended {
		t.Fatalf("recommendation: %+v", out.Options)
	}
	after, _ := json.Marshal(options)
	if request.Question != deploySample || string(before) != string(after) {
		t.Fatalf("template mutated source: %s %s", before, after)
	}
	options.Options[1].Recommended = true
	request.Reason = "producer reason"
	out, ok = RenderDecisionTemplate(request, &options)
	if !ok || out.Options.Options[0].Recommended || !out.Options.Options[1].Recommended || !strings.Contains(out.Reason, "요청자 권고: producer reason") {
		t.Fatalf("producer recommendation lost: %+v", out)
	}
}

func TestMergewakeDeployTemplateReviewAndStrictParsing(t *testing.T) {
	options := DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}
	for _, change := range []struct{ from, to string }{{`"paths":[]`, `"paths":["deploy/schema.sql"]`}, {`"missing or invalid previous deployment receipt"`, `"unknown reason"`}, {`"environment_known":true`, `"environment_known":false`}} {
		r := DecisionRequest{RequestedBy: "mergewake-design", Question: strings.Replace(deploySample, change.from, change.to, 1)}
		got, ok := RenderDecisionTemplate(r, &options)
		if !ok || got.Options.Options[0].Recommended || !strings.Contains(got.Reason, "검토 필요") {
			t.Fatalf("unsafe recommendation for %s: %+v", change.to, got)
		}
		if strings.Contains(change.to, "unknown reason") && !strings.Contains(got.Question, "unknown reason") {
			t.Fatal("unknown reason dropped")
		}
	}
	for _, question := range []string{deploySample + " {}", strings.Replace(deploySample, `"host":"mbp"`, `"host":"mbp","extra":true`, 1), strings.Replace(deploySample, `"paths":[]`, `"paths":[42]`, 1)} {
		if _, ok := RenderDecisionTemplate(DecisionRequest{RequestedBy: "mergewake-design", Question: question}, &options); ok {
			t.Fatalf("matched malformed payload: %s", question)
		}
	}
	if _, ok := RenderDecisionTemplate(DecisionRequest{RequestedBy: "other", Question: deploySample}, &options); ok {
		t.Fatal("matched wrong producer")
	}
}

func TestDecisionTemplateLeavesStoredRequestIdentical(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := t.Context()
	task := decisionTask(t, s, "backlog")
	in := DecisionRequestInput{Question: deploySample, Options: DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}, DefaultAction: "Hold; keep the current version"}
	result, err := s.RecordDecisionRequest(ctx, task.ID, "mergewake-design", in)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := RenderDecisionTemplate(result.Request, result.Task.Refs.DecisionOptions)
	if !ok {
		t.Fatal("template did not match")
	}
	got, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("get task: found=%v err=%v", found, err)
	}
	before, _ := json.Marshal(in.Options)
	after, _ := json.Marshal(got.Refs.DecisionOptions)
	if got.Refs.DecisionRequest.Question != in.Question || string(before) != string(after) {
		t.Fatalf("stored request changed: %q %s", got.Refs.DecisionRequest.Question, after)
	}
	if err := s.PutTranslation(ctx, Translation{Entity: "decision_request", EntityID: result.Request.ID, Field: "question", Lang: "ko", SourceSHA256: SourceSHA256(in.Question), Text: "translation job output", Translator: "test"}); err != nil {
		t.Fatal(err)
	}
	translations, err := s.ListTranslations(ctx, "decision_request", []TranslationSource{{EntityID: result.Request.ID, Field: "question", Original: in.Question}})
	if err != nil || len(translations) != 1 || translations[0].Stale || !strings.Contains(translations[0].Text, "대상 호스트") {
		t.Fatalf("translations=%+v err=%v", translations, err)
	}
	missing, err := s.MissingDecisionTranslations(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range missing {
		if m.EntityID == result.Request.ID {
			t.Fatalf("templated request queued for translation: %+v", m)
		}
	}
	retry, err := s.RecordDecisionRequest(ctx, task.ID, "mergewake-design", in)
	if err != nil || !retry.Duplicate {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}
