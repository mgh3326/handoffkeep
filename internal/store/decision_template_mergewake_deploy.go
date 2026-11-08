package store

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

func mergewakeDeployTemplate(r DecisionRequest, options *DecisionOptions) (DecisionTemplate, bool) {
	if r.RequestedBy != "mergewake-design" || options == nil || len(options.Options) != 2 || options.Options[0].Key != "A" || options.Options[0].Label != "Approve this deployment" || options.Options[1].Key != "B" || options.Options[1].Label != "Keep the current version" {
		return DecisionTemplate{}, false
	}
	raw, ok := strings.CutPrefix(r.Question, "Approve deployment ")
	if !ok {
		return DecisionTemplate{}, false
	}
	var p struct {
		Identity *struct {
			EnvironmentKnown *bool   `json:"environment_known"`
			Config           *string `json:"config"`
			Payload          *string `json:"payload"`
		} `json:"identity"`
		Host     *string   `json:"host"`
		Repo     *string   `json:"repo"`
		Previous *string   `json:"previous"`
		Release  *string   `json:"release"`
		Paths    *[]string `json:"paths"`
		Reasons  *[]string `json:"reasons"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF || p.Identity == nil || p.Identity.EnvironmentKnown == nil || p.Identity.Config == nil || p.Identity.Payload == nil || p.Host == nil || *p.Host == "" || p.Repo == nil || *p.Repo == "" || p.Previous == nil || p.Release == nil || *p.Release == "" || p.Paths == nil || p.Reasons == nil {
		return DecisionTemplate{}, false
	}
	reasons := make([]string, 0, len(*p.Reasons))
	safe := len(*p.Paths) == 0 && *p.Identity.EnvironmentKnown && *p.Previous == "unknown"
	hasReceiptReason := false
	for _, reason := range *p.Reasons {
		switch reason {
		case "host inventory approval missing or changed":
			reasons = append(reasons, "이 호스트의 배포 승인 기록이 없거나 바뀜")
		case "missing or invalid previous deployment receipt":
			hasReceiptReason = true
			reasons = append(reasons, "이전 배포 기록이 없음")
		default:
			reasons = append(reasons, reason)
			safe = false
		}
	}
	sha := *p.Release
	if len(sha) > 7 {
		sha = sha[:7]
	}
	out := DecisionTemplate{Options: *options, Reason: r.Reason}
	out.Options.Options = append([]DecisionOption(nil), options.Options...)
	out.Options.Options[0].Label = "이 배포 승인"
	out.Options.Options[1].Label = "현재 버전 유지"
	producer := false
	for _, o := range options.Options {
		producer = producer || o.Recommended
	}
	if producer {
		out.Reason = "요청자 권고: " + r.Reason
	} else if safe && hasReceiptReason {
		out.Options.Options[0].Recommended = true
		out.Reason = "hk 규칙: 승인이 필요한 경로 없음 · 이전 배포 기록 없음. 호스트 승인 기록 없음/변경은 처음 켜는 경우와 설정 변경을 구분할 수 없음 — 호스트 설정을 바꾸지 않았다면 A"
	} else {
		out.Reason = "검토 필요: 경로 및 사유를 확인하세요"
	}
	if !producer && r.Reason != "" {
		out.Reason += " · 요청자 사유: " + r.Reason
	}
	details, err := json.Marshal(struct {
		Host           string   `json:"대상 호스트"`
		Repo           string   `json:"저장소"`
		Release        string   `json:"릴리스"`
		Previous       string   `json:"이전 배포"`
		Paths          []string `json:"승인이 필요한 경로"`
		Reasons        []string `json:"사유"`
		Recommendation string   `json:"판단"`
	}{*p.Host, *p.Repo, *p.Release, *p.Previous, *p.Paths, reasons, out.Reason})
	if err != nil {
		return DecisionTemplate{}, false
	}
	out.Question = fmt.Sprintf("%s %s을(를) %s에 설치할까요? %s", path.Base(*p.Repo), sha, *p.Host, details)
	return out, true
}
