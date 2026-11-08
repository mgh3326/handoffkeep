package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestProjectDecisionTemplate(t *testing.T) {
	q := `Approve deployment {"identity":{"environment_known":true,"config":"x","payload":"y"},"host":"mbp","repo":"mgh3326/handoffkeep","previous":"unknown","release":"d4b91c069","paths":[],"reasons":["missing or invalid previous deployment receipt"]}`
	options := &store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}
	req := store.DecisionRequest{ID: "dr-1-1", Status: store.DecisionRequestOpen, RequestedBy: "mergewake-design", Question: q}
	v := projectDecisionRequest(store.DecisionRequestEntry{Request: req, Options: options, Current: true}, "backlog", time.Now())
	if v.Question != q || !strings.Contains(v.QuestionKO, "대상 호스트") || !v.Options[0].Recommended || v.Options[0].Label != options.Options[0].Label || v.OptionsKO[0].Label != "이 배포 승인" || !strings.Contains(v.Reason, "hk 규칙") {
		t.Fatalf("view=%+v", v)
	}
	if options.Options[0].Recommended {
		t.Fatal("mutated source options")
	}
}
