package ytwrite

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Kind is the YouTrack side of one op kind (comment_append, ...): how its text
// is rendered, how it is sent, and how evidence of it is found. The protocol
// owns everything else (claim, ordering, retries, the applied/unknown
// decision), the same for every kind. A kind never decides an op is applied.
type Kind interface {
	// Render is the exact text written for op: deterministic in the op (a
	// resend or a reconciliation renders the same bytes) and carrying
	// OpMarker(op.ID). An error means the op cannot be sent.
	Render(op Op) (string, error)
	// Send makes exactly one remote attempt to write text for op and returns
	// the created YouTrack id. No retry inside: a retry after a lost reply
	// could duplicate the write. Errors follow Remote (a *RemoteError is an
	// HTTP answer, anything else is a transport failure).
	Send(ctx context.Context, remote Remote, op Op, text string) (ytID string, err error)
	// Evidence enumerates op's target completely and classifies every
	// candidate that carries OpMarker(op.ID) against text. On a failed
	// enumeration it returns the error, with RetryAfter set from a 429.
	Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error)
	// MapRows is the hk identity -> YouTrack id rows committed with op's
	// applied, when ytID is the created (or adopted) YouTrack id. The send,
	// evidence and operator paths all apply through it.
	MapRows(op Op, ytID string) []MapRow
}

// RefusedError is a kind's refusal in Send before it wrote anything (a read
// it made showed the write must not happen): the protocol rejects the op with
// Reason. It is never returned after a write request left.
type RefusedError struct {
	Reason string
	Detail string
}

func (e *RefusedError) Error() string { return e.Reason + ": " + e.Detail }

// AdoptedError is not a failure: Send found the effect already present (an
// issue made before this op) and adopts it without writing. The protocol
// applies the op with ID and marks its map rows Adopted, so the store can
// tell an adopted thing, which may already hold history, from a created
// one.
type AdoptedError struct {
	ID string
}

func (e *AdoptedError) Error() string { return "adopted " + e.ID }

// NotSentError is a read in Send that failed before the write request left:
// nothing was written, so the op goes back to prepared (a 4xx read is
// rejected). Err is the read's error (a *RemoteError or a transport failure).
type NotSentError struct {
	Err error
}

func (e *NotSentError) Error() string { return "not sent: " + e.Err.Error() }
func (e *NotSentError) Unwrap() error { return e.Err }

// Evidence is what one search of an op's target found: the candidates that
// carry the op's marker, by how they compare with the rendered text.
type Evidence struct {
	// Matches are the ids of live candidates whose text is exactly the
	// rendered text.
	Matches []string
	// Deleted counts exact candidates that were deleted; Mismatched counts
	// candidates carrying the marker with other text, deleted or not.
	Deleted    int
	Mismatched int
	// RetryAfter is the wait a rate-limited (429) search asked for; the next
	// search waits at least this long.
	RetryAfter time.Duration
}

// kindBuilders are the registered kinds, by op kind. Each kind registers
// itself from its own file's init, so kinds added in parallel edit no shared
// list.
var kindBuilders = map[string]func(Config) Kind{}

// registerKind adds a kind to every Protocol New builds. build gets the
// filled Config (PageSize and the like).
func registerKind(kind string, build func(Config) Kind) {
	if _, dup := kindBuilders[kind]; dup || kind == "" {
		panic(fmt.Sprintf("ytwrite: kind %q registered twice or empty", kind))
	}
	kindBuilders[kind] = build
}

// kindFor is op kind's Kind on p: the registered instance, or one built from
// the registry for a Protocol made without New (the operator's resolve).
func (p *Protocol) kindFor(kind string) (Kind, bool) {
	if k, ok := p.Kinds[kind]; ok {
		return k, true
	}
	build, ok := kindBuilders[kind]
	if !ok {
		return nil, false
	}
	return build(p.Config), true
}

// RegisteredKinds lists every kind New registers on a Protocol, sorted. The
// store checks its per-kind admission gates against it.
func RegisteredKinds() []string {
	out := make([]string, 0, len(kindBuilders))
	for k := range kindBuilders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
