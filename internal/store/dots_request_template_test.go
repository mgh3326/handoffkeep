package store

import (
	"strings"
	"testing"
)

const (
	dotsTemplateInput = "[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드, RefImport 설치됨"
	dotsTemplateBody  = "[목적] 홈 화면 시안\n[기준: 정본 Canva 3페이지]\n[유지할 것] 색상 체계\n[범위] 1화면, 기본 상태\n[제약] 없음\n[산출물] PNG 2x\n[완료 기준] 채택 표시\n" + dotsTemplateInput
)

// withDotsInput is dotsTemplateBody with its [입력] line replaced.
func withDotsInput(input string) string {
	return strings.Replace(dotsTemplateBody, dotsTemplateInput, input, 1)
}

func TestCheckDotsRequestTemplate(t *testing.T) {
	cases := []struct {
		name string
		in   DotsRequestInput
		ok   bool
		// wantErr is a substring the refusal must contain when ok is false.
		wantErr string
	}{
		{"untyped research request needs no template", DotsRequestInput{Body: "Please investigate"}, true, ""},
		{"typed request with every section, SHA and environment", DotsRequestInput{Type: DotsRequestCreateMockup, Body: dotsTemplateBody}, true, ""},
		{"REVIEW_IMPLEMENTATION takes the same template", DotsRequestInput{Type: DotsRequestReviewImplementation, Body: dotsTemplateBody}, true, ""},
		{"CREATE_ASSET made from a build", DotsRequestInput{Type: DotsRequestCreateAsset, Body: dotsTemplateBody}, true, ""},
		{"unknown type is refused", DotsRequestInput{Type: "CREATE_PHOTO", Body: dotsTemplateBody}, false, "invalid dots request type"},
		{"a missing section is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: strings.Replace(dotsTemplateBody, "[제약] 없음\n", "", 1)}, false, ""},
		{"[입력] without a commit SHA is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "9f14c82", "최신 커밋", 1)}, false, "names no commit SHA"},
		{"a hex word with no digit is not a commit SHA", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "9f14c82", "acceded", 1)}, false, "names no commit SHA"},
		{"[입력] without a capture environment is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "캡처 환경: 로컬 빌드, RefImport 설치됨", "스크린샷 첨부", 1)}, false, "names no capture environment"},
		{"[입력] split over lines with SHA and environment", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, dotsTemplateInput, "[입력]\n커밋 9f14c82\n캡처 환경: 로컬 빌드", 1)}, true, ""},
		{"a section with no content is refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: strings.Replace(dotsTemplateBody, "[제약] 없음\n", "[제약]\n", 1)}, false, ""},
		{"labels in one line are not sections", DotsRequestInput{Type: DotsRequestCreateMockup, Body: "[목적] [유지할 것] [범위] [제약] [산출물] [완료 기준] [입력] 커밋 9f14c82, 캡처 환경: 로컬"}, false, ""},
		{"[입력] inside another section is not a section", DotsRequestInput{Type: DotsRequestCreateMockup, Body: "[목적] 홈 화면 시안 [입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드\n[유지할 것] 색상 체계\n[범위] 1화면, 기본 상태\n[제약] 없음\n[산출물] PNG 2x\n[완료 기준] 채택 표시"}, false, ""},

		// CREATE_ASSET may name source material instead of a build (operator
		// decisions 347 and 349).
		{"CREATE_ASSET from an hk attachment sha256", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 첨부 sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")}, true, ""},
		{"CREATE_ASSET from an hk doc key", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 원본 자료: hk:doc consult/2026-11-16/dots-request-meshy-1")}, true, ""},
		{"CREATE_ASSET from a 원본: description", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 원본: dashcam-2026-11-14.mp4 00:12-00:48, 참고 사진 3장")}, true, ""},
		{"CREATE_ASSET source material needs no capture environment", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] hk:doc consult/2026-11-16/dots-request-meshy-1")}, true, ""},
		{"CREATE_ASSET source material beside a commit without environment", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력]\n커밋 9f14c82\n원본: dashcam-2026-11-14.mp4 00:12-00:48")}, true, ""},
		{"CREATE_ASSET with neither a build nor source material is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 최신 빌드 화면")}, false, "names neither a build"},
		{"CREATE_ASSET with a commit but no environment is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 커밋 9f14c82")}, false, "capture environment"},
		{"CREATE_ASSET with a build path but no build is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] Assets/RefImport 설치됨")}, false, "names neither a build"},
		{"a 원본: marker with a short description is refused", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 원본: 사진")}, false, "names neither a build"},
		{"an hk:doc key needs two segments", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] hk:doc dots-request-meshy-1")}, false, "names neither a build"},
		{"source: inside prose is not a marker", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력] 최신 빌드 화면 (open source: github.com/acme/tools)")}, false, "names neither a build"},
		{"a marker's description does not run past its line", DotsRequestInput{Type: DotsRequestCreateAsset, Body: withDotsInput("[입력]\n원본: 사진\n참고 3장 설명 추가")}, false, "names neither a build"},
		{"CREATE_MOCKUP with source material is still refused", DotsRequestInput{Type: DotsRequestCreateMockup, Body: withDotsInput("[입력] 원본: dashcam-2026-11-14.mp4 00:12-00:48")}, false, "names no commit SHA"},
		{"REVIEW_IMPLEMENTATION with source material is still refused", DotsRequestInput{Type: DotsRequestReviewImplementation, Body: withDotsInput("[입력] 첨부 sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")}, false, "names no commit SHA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDotsRequestTemplate(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok = %v", err, tc.ok)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
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
