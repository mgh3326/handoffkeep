package store

import (
	"errors"
	"regexp"
	"strings"
)

// Request types a design session files with `dots request --type` (hk:doc
// policy/dots-on-hk, section 제작 요청). A request with no type is a free-form
// request such as research, and carries no template.
const (
	DotsRequestCreateMockup         = "CREATE_MOCKUP"
	DotsRequestCreateAsset          = "CREATE_ASSET"
	DotsRequestReviewImplementation = "REVIEW_IMPLEMENTATION"
)

// dotsRequestSections are the labels the policy template requires in a typed
// body. [기준] sits inside [목적], so it is not a label of its own.
var dotsRequestSections = []string{"[목적]", "[유지할 것]", "[범위]", "[제약]", "[산출물]", "[완료 기준]", "[입력]"}

var dotsCommitTokenRE = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// checkDotsRequestTemplate refuses a typed request whose body lacks the policy
// template. A request without its [입력] commit SHA and capture environment
// cannot be reproduced, so it is refused rather than filed and flagged. The
// SHA is a 7 to 40 character hex token with at least one digit, so a plain
// word such as "acceded" does not pass as a commit.
func checkDotsRequestTemplate(in DotsRequestInput) error {
	if in.Type == "" {
		return nil
	}
	switch in.Type {
	case DotsRequestCreateMockup, DotsRequestCreateAsset, DotsRequestReviewImplementation:
	default:
		return errors.New("invalid dots request type: CREATE_MOCKUP, CREATE_ASSET or REVIEW_IMPLEMENTATION")
	}
	for _, label := range dotsRequestSections {
		if !strings.Contains(in.Body, label) {
			return errors.New("invalid dots request: the body lacks the " + label + " section")
		}
	}
	input := dotsRequestSection(in.Body, "[입력]")
	if !hasDotsCommitToken(input) {
		return errors.New("invalid dots request: [입력] names no commit SHA")
	}
	lower := strings.ToLower(input)
	if !strings.Contains(lower, "환경") && !strings.Contains(lower, "environment") {
		return errors.New("invalid dots request: [입력] names no capture environment")
	}
	return nil
}

// dotsRequestSection returns the text after label up to the next line that
// starts a new bracket section.
func dotsRequestSection(body, label string) string {
	_, rest, _ := strings.Cut(body, label)
	section, _, _ := strings.Cut(rest, "\n[")
	return section
}

func hasDotsCommitToken(s string) bool {
	for _, tok := range dotsCommitTokenRE.FindAllString(s, -1) {
		if strings.ContainsAny(tok, "0123456789") {
			return true
		}
	}
	return false
}

// dotsRequestBody is the body the request task stores. A typed request leads
// with its [유형] line so the inbox shows what was asked.
func dotsRequestBody(in DotsRequestInput) string {
	if in.Type == "" {
		return in.Body
	}
	return "[유형] " + in.Type + "\n" + in.Body
}
