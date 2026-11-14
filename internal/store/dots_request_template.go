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

// dotsRequestLabels are the labels the policy template requires in a typed
// body. [기준] sits inside [목적], so it is not a label of its own.
var dotsRequestLabels = []string{"[목적]", "[유지할 것]", "[범위]", "[제약]", "[산출물]", "[완료 기준]", "[입력]"}

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
	sections := dotsRequestSections(in.Body)
	for _, label := range dotsRequestLabels {
		if strings.TrimSpace(sections[label]) == "" {
			return errors.New("invalid dots request: the body lacks a " + label + " section with content")
		}
	}
	input := sections["[입력]"]
	if !hasDotsCommitToken(input) {
		return errors.New("invalid dots request: [입력] names no commit SHA")
	}
	lower := strings.ToLower(input)
	if !strings.Contains(lower, "환경") && !strings.Contains(lower, "environment") {
		return errors.New("invalid dots request: [입력] names no capture environment")
	}
	return nil
}

// dotsRequestSections splits a body into its policy sections. A section starts
// at a line whose text begins with one of the labels and runs to the next such
// line. A label inside a line starts nothing, so a label quoted in prose does
// not count as a section.
func dotsRequestSections(body string) map[string]string {
	sections := map[string]string{}
	current := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if label := dotsRequestLabelAt(trimmed); label != "" {
			current = label
			line = strings.TrimPrefix(trimmed, label)
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}

// dotsRequestLabelAt returns the template label that line begins with, or "".
func dotsRequestLabelAt(line string) string {
	for _, label := range dotsRequestLabels {
		if strings.HasPrefix(line, label) {
			return label
		}
	}
	return ""
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
