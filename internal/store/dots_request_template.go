package store

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
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

var (
	dotsCommitTokenRE = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	// dotsAttachmentSHA256RE matches the content hash of an hk attachment: 64
	// hex characters, which dotsCommitTokenRE deliberately does not take.
	dotsAttachmentSHA256RE = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)
	// dotsSourceMarkerRE matches the marker that starts a source-material
	// line: "원본:" or "source:", or the bracketed [원본]/[source], at the
	// start of a line apart from leading spaces. Anchoring it to the line
	// keeps a "source:" inside prose ("open source: ...") from counting.
	dotsSourceMarkerRE = regexp.MustCompile(`(?im)^[ \t]*(?:\[(?:원본|source)\]|(?:원본|source)\s*[:：])`)
	// dotsDocMentionRE matches an hk document reference in the "hk:doc <key>"
	// notation the policy documents use, capturing the key.
	dotsDocMentionRE = regexp.MustCompile(`(?i)\bhk:doc[ \t]+([A-Za-z0-9._/-]+)`)
)

// dotsSourceDescriptionRunes is the shortest source-material description that
// counts: enough for a file name with its span, not a two-character word.
const dotsSourceDescriptionRunes = 8

// checkDotsRequestTemplate refuses a typed request whose body lacks the policy
// template. The six sections other than [입력] are required for every type.
//
// A [입력] must say what the work starts from. For CREATE_MOCKUP and
// REVIEW_IMPLEMENTATION that is a build: a commit SHA (7 to 40 hex characters
// with at least one digit, so a plain word such as "acceded" does not pass as
// a commit) and the capture environment, which together make the request
// reproducible. CREATE_ASSET may instead name the source material the asset is
// made from, which stands in for both the commit and the capture environment
// (operator decisions 347 and 349). Source material is one of:
//   - an hk attachment sha256: a 64-character hex token;
//   - an hk document key in "hk:doc <key>" notation: at least two non-empty
//     "/"-separated segments in the document-key alphabet;
//   - a line that starts with "원본:" or "source:" (or [원본]/[source]) and
//     describes the material in at least dotsSourceDescriptionRunes non-space
//     characters on that line, for a file or video that is not in hk yet.
//
// The sha256 and the key are checked for shape, not for existence: this
// function sees no store, so a well-formed hash or key that names nothing is a
// data-quality problem for the request, not a refusal here.
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
	if in.Type == DotsRequestCreateAsset {
		if (hasDotsCommitToken(input) && hasDotsCaptureEnvironment(input)) || hasDotsSourceMaterial(input) {
			return nil
		}
		return errors.New("invalid dots request: [입력] names neither a build (commit SHA and capture environment) nor source material (an hk attachment sha256, an \"hk:doc\" key, or a \"원본:\" line with a description)")
	}
	if !hasDotsCommitToken(input) {
		return errors.New("invalid dots request: [입력] names no commit SHA")
	}
	if !hasDotsCaptureEnvironment(input) {
		return errors.New("invalid dots request: [입력] names no capture environment")
	}
	return nil
}

// hasDotsCaptureEnvironment reports whether s names the environment a build
// was captured in.
func hasDotsCaptureEnvironment(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "환경") || strings.Contains(lower, "environment")
}

// hasDotsSourceMaterial reports whether a [입력] section names the material an
// asset is made from instead of a build, in one of the forms listed on
// checkDotsRequestTemplate.
func hasDotsSourceMaterial(s string) bool {
	if dotsAttachmentSHA256RE.MatchString(s) {
		return true
	}
	for _, ref := range dotsDocMentionRE.FindAllStringSubmatch(s, -1) {
		if isDotsDocKey(ref[1]) {
			return true
		}
	}
	for _, marker := range dotsSourceMarkerRE.FindAllStringIndex(s, -1) {
		// The description is the rest of the marker's own line; a later line
		// is not part of it.
		description := s[marker[1]:]
		if newline := strings.IndexByte(description, '\n'); newline >= 0 {
			description = description[:newline]
		}
		if dotsNonSpaceRunes(description) >= dotsSourceDescriptionRunes {
			return true
		}
	}
	return false
}

// isDotsDocKey reports whether key has the shape of an hk document key: two or
// more non-empty "/"-separated segments in the document-key alphabet, and the
// store's own key contract. A build path such as "Assets/RefImport" is not a
// doc key, which is why source material names a document as "hk:doc <key>".
func isDotsDocKey(key string) bool {
	segments := strings.Split(key, "/")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" {
			return false
		}
	}
	return ValidDocKey(key)
}

// dotsNonSpaceRunes counts the non-space characters of s.
func dotsNonSpaceRunes(s string) int {
	return utf8.RuneCountInString(strings.Join(strings.Fields(s), ""))
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
