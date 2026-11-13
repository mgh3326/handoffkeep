package ytwrite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ProvenanceTimeLayout is the UTC time in a provenance line. REST cannot set a
// comment's author or created time, so every hk-written comment says who wrote
// it and when in its first line (hk 3667 v2.1).
const ProvenanceTimeLayout = "2006-01-02T15:04:05Z"

// ProvenanceLine is the first line of every comment hk writes:
//
//	작성: <actor> · <UTC time> · hk op <id>
//
// The time is the op's creation time, so a resend or a reconciliation renders
// the same bytes.
func ProvenanceLine(actor string, at time.Time, opID int64) string {
	return fmt.Sprintf("작성: %s · %s · hk op %d", actor, at.UTC().Format(ProvenanceTimeLayout), opID)
}

var provenanceRE = regexp.MustCompile(`^작성: (.+) · (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z) · hk op ([0-9]+)$`)

// ParseProvenance reads a provenance line back. ok is false for any other
// text. An actor is validated to hold no " · ", so the split is unambiguous.
func ParseProvenance(line string) (actor string, at time.Time, opID int64, ok bool) {
	m := provenanceRE.FindStringSubmatch(line)
	if m == nil {
		return "", time.Time{}, 0, false
	}
	t, err := time.Parse(ProvenanceTimeLayout, m[2])
	if err != nil {
		return "", time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil || id <= 0 {
		return "", time.Time{}, 0, false
	}
	return m[1], t, id, true
}

// markerPrefix opens every hk op marker. A caller body may not contain it, so
// a marker in a remote comment is evidence only when the whole text equals what
// hk rendered.
const markerPrefix = "<!-- hk-op:"

// OpMarker is the trailing HTML comment of a comment body.
func OpMarker(opID int64) string { return markerPrefix + strconv.FormatInt(opID, 10) + " -->" }

// RenderedOpID reads the op id from a text in exactly the form RenderComment
// renders: a provenance line naming op N, a blank line, a body a caller could
// have sent (no marker in it), a blank line and op N's marker as the very end.
// ok is false for anything else, including a text that only quotes a marker.
// The form alone is not ownership: a reader must also find op N in its own
// ledger, bound to the same target.
func RenderedOpID(text string) (opID int64, ok bool) {
	first, rest, found := strings.Cut(text, "\n\n")
	if !found {
		return 0, false
	}
	actor, _, id, ok := ParseProvenance(first)
	if !ok || checkActor(actor) != nil {
		return 0, false
	}
	body, found := strings.CutSuffix(rest, "\n\n"+OpMarker(id))
	// A document part (doc_comment.go) is longer than a hk comment; both
	// forms render the same way, so either is accepted here.
	if !found || checkBodyMax(body, textBodyLimit) != nil {
		return 0, false
	}
	return id, true
}

// CommentPayload is the immutable outbound payload of a comment_append op.
type CommentPayload struct {
	Body string `json:"body"`
}

// RenderComment is the exact text sent to YouTrack for a comment_append op: the
// provenance line, the caller's body, the op marker.
func RenderComment(op Op) (string, error) {
	var p CommentPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return "", fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	text := renderCommentText(op.Actor, op.CreatedAt, op.ID, p.Body)
	// Admission refuses every body whose text would not fit (CheckComment,
	// CheckCommentContent). This is the backstop: refuse by name, never send
	// a cut or failed write.
	if !WithinTextMax(text) {
		return "", fmt.Errorf("%w: rendered comment is over %d characters (YouTrack's text bound)", ErrInvalid, TextMaxChars)
	}
	return text, nil
}

// renderCommentText is the exact text of a comment: the provenance line, the
// body, the op marker. The op id is part of the text, so its length is part of
// what the bound counts.
func renderCommentText(actor string, at time.Time, opID int64, body string) string {
	return ProvenanceLine(actor, at, opID) + "\n\n" + body + "\n\n" + OpMarker(opID)
}

// worstOpID is the largest op id: 19 decimal digits, the longest the provenance
// line and the marker can carry. An op id is only known once the op is stored,
// so admission measures with this id; a text that fits with it fits with any.
const worstOpID = math.MaxInt64

// commentFitsAnyOpID reports whether a comment by actor with body is sendable
// under every op id, so admission refuses what Render would refuse later.
func commentFitsAnyOpID(actor, body string) bool {
	return WithinTextMax(renderCommentText(actor, time.Time{}, worstOpID, body))
}

const maxIdentLen = 200

var (
	principalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,199}$`)
	keyRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/=+-]{0,199}$`)
	issueIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// checkActor explains a refused actor: one line, no " · " (the provenance
// separator), no control characters.
func checkActor(actor string) error {
	if actor == "" || len(actor) > maxIdentLen {
		return fmt.Errorf("%w: actor must be 1-%d bytes", ErrInvalid, maxIdentLen)
	}
	if strings.Contains(actor, " · ") || strings.TrimSpace(actor) != actor {
		return fmt.Errorf("%w: actor must not contain the provenance separator or edge spaces", ErrInvalid)
	}
	if !utf8.ValidString(actor) {
		return fmt.Errorf("%w: actor is not valid UTF-8", ErrInvalid)
	}
	for _, r := range actor {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: actor must not contain control characters", ErrInvalid)
		}
	}
	return nil
}

// checkBody refuses an empty body, an over-size body, and a body that carries
// the reserved marker text.
func checkBody(body string) error { return checkBodyMax(body, textBodyLimit) }

// bodyLimit is the size rule of one body: a count of characters (a YouTrack
// text field, see bounds.go) or of UTF-8 bytes (a document part).
type bodyLimit struct {
	max   int
	chars bool
}

// textBodyLimit is YouTrack's text bound for a comment body or an issue text.
var textBodyLimit = bodyLimit{max: TextMaxChars, chars: true}

// docPartBodyLimit is one document-comment part, which the import split at
// DocCommentPartBytes.
var docPartBodyLimit = bodyLimit{max: MaxDocCommentBodyBytes}

func (l bodyLimit) over(s string) bool {
	if l.chars {
		return utf8.RuneCountInString(s) > l.max
	}
	return len(s) > l.max
}

func (l bodyLimit) describe() string {
	if l.chars {
		return fmt.Sprintf("%d characters (YouTrack's text bound)", l.max)
	}
	return fmt.Sprintf("%d bytes", l.max)
}

// checkBodyMax is checkBody with the size rule given.
func checkBodyMax(body string, lim bodyLimit) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: body is empty", ErrInvalid)
	}
	if lim.over(body) {
		return fmt.Errorf("%w: body is over %s", ErrInvalid, lim.describe())
	}
	// jsonb cannot hold NUL, and a body that is not UTF-8 would not survive
	// the JSON payload unchanged.
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return fmt.Errorf("%w: body is not valid UTF-8 text without NUL", ErrInvalid)
	}
	if strings.Contains(body, markerPrefix) {
		return fmt.Errorf("%w: body contains the reserved hk op marker", ErrInvalid)
	}
	return nil
}

// payloadSHA hashes what a comment_append means, the tenant included: the same
// key bound to another YouTrack is another write. The fields are length-framed
// so two different tuples cannot concatenate to the same bytes.
func payloadSHA(kind, entityKind, entityID, ytTarget, tenant, actor, body string) string {
	h := sha256.New()
	for _, f := range []string{kind, entityKind, entityID, ytTarget, tenant, actor, body} {
		fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}
