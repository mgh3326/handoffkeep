package store

import (
	"strings"
	"testing"
)

const dotsTemplateBody = "[목적] 홈 화면 시안\n[기준: 정본 Canva 3페이지]\n[유지할 것] 색상 체계\n[범위] 1화면, 기본 상태\n[제약] 없음\n[산출물] PNG 2x\n[완료 기준] 채택 표시\n[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드, RefImport 설치됨"

func TestCheckDotsRequestTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   DotsRequestInput
		ok   bool
	}{
		{"untyped research request needs no template", DotsRequestInput{Body: "Please investigate"}, true},
		{"typed request with every section, SHA and environment", DotsRequestInput{Type: DotsRequestCreateMockup, Body: dotsTemplateBody}, true},
		{"REVIEW_IMPLEMENTATION takes the same template", DotsRequestInput{Type: DotsRequestReviewImplementation, Body: dotsTemplateBody}, true},
		{"unknown type is refused", DotsRequestInput{Type: "CREATE_PHOTO", Body: dotsTemplateBody}, false},
		{"a missing section is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: strings.Replace(dotsTemplateBody, "[제약] 없음\n", "", 1)}, false},
		{"[입력] without a commit SHA is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "9f14c82", "최신 커밋", 1)}, false},
		{"a hex word with no digit is not a commit SHA", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "9f14c82", "acceded", 1)}, false},
		{"[입력] without a capture environment is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "캡처 환경: 로컬 빌드, RefImport 설치됨", "스크린샷 첨부", 1)}, false},
		{"[입력] split over lines with SHA and environment", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드, RefImport 설치됨", "[입력]\n커밋 9f14c82\n캡처 환경: 로컬 빌드", 1)}, true},
		{"a section with no content is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "[제약] 없음\n", "[제약]\n", 1)}, false},
		{"labels in one line are not sections", DotsRequestInput{Type: DotsRequestCreateMockup, Body: "[목적] [유지할 것] [범위] [제약] [산출물] [완료 기준] [입력] 커밋 9f14c82, 캡처 환경: 로컬"}, false},
		{"[입력] inside another section is not a section", DotsRequestInput{Type: DotsRequestCreateMockup, Body: "[목적] 홈 화면 시안 [입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드\n[유지할 것] 색상 체계\n[범위] 1화면, 기본 상태\n[제약] 없음\n[산출물] PNG 2x\n[완료 기준] 채택 표시"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDotsRequestTemplate(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

func TestDotsRequestBodyLeadsTypedRequestWithItsType(t *testing.T) {
	if got := dotsRequestBody(DotsRequestInput{Body: "Please investigate"}); got != "Please investigate" {
		t.Fatalf("untyped body = %q, want it unchanged", got)
	}
	got := dotsRequestBody(DotsRequestInput{Type: DotsRequestCreateAsset, Body: dotsTemplateBody})
	if got != "[유형] CREATE_ASSET\n"+dotsTemplateBody {
		t.Fatalf("typed body = %q", got)
	}
}
