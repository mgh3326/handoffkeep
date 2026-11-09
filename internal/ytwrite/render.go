package ytwrite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	return ProvenanceLine(op.Actor, op.CreatedAt, op.ID) + "\n\n" + p.Body + "\n\n" + OpMarker(op.ID), nil
}

const (
	// MaxBodyBytes is hk's own cap on one comment body; YouTrack's limit is
	// not documented for the tenant, so hk refuses early rather than learn it
	// from a failed write.
	MaxBodyBytes = 64 << 10
	maxIdentLen  = 200
)

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
func checkBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: body is empty", ErrInvalid)
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body is over %d bytes", ErrInvalid, MaxBodyBytes)
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

// payloadSHA hashes what a comment_append means. The fields are length-framed
// so two different tuples cannot concatenate to the same bytes.
func payloadSHA(kind, entityKind, entityID, ytTarget, body string) string {
	h := sha256.New()
	for _, f := range []string{kind, entityKind, entityID, ytTarget, body} {
		fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}
