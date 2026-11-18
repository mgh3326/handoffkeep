package ytwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The article_split kind (hk 5085, design/2026-11-18/hk5085-yt-split): a
// document over MaxArticleBytes is mirrored as one index article and n part
// articles under it, through one op. The op's marker is the index's marker
// (OpMarker); part i carries PartMarker(op, i, n). Like issue_links it is a
// planned kind (protocol_split.go): every send and recovery pass reads the
// family, commits a plan of the creates the read showed missing, marks each
// create in the ledger before its request, and reads again. Unlike a link, an
// article is never deduplicated by YouTrack, so the read that decides a create
// is a direct one: the index by its recorded id (the op's YTResultID, written
// before the first part is requested), its children by
// ReadArticle(index).ChildIDs and each child by id, never a search. The one
// search left is for an index whose id was never recorded; its absence is
// not proof, so it never leads to a second index (IndexMaybeSent).

func init() {
	registerKind(KindArticleSplit, func(cfg Config) Kind { return articleSplit{create: articleCreate{ledger: cfg.Ledger}} })
}

// KindArticleSplit creates a long document's index article and its parts.
const KindArticleSplit = "article_split"

// splitPartOverhead bounds what a part's rendering adds to its chunk: the
// provenance line (an actor of at most maxIdentLen bytes), two blank lines and
// the part marker.
const splitPartOverhead = 1 << 10

// SplitChunkMaxBytes is the most bytes of the escaped body one part carries,
// so a rendered part stays within ArticlePartMaxBytes.
const SplitChunkMaxBytes = ArticlePartMaxBytes - splitPartOverhead

// NeedsSplit reports whether a document body is written as an index and parts.
func NeedsSplit(body string) bool { return len(body) > MaxArticleBytes }

// SplitArticleBody cuts the escaped body (EscapeArticleBody) into the parts'
// chunks: each at most SplitChunkMaxBytes, cut after the last line end in its
// second half when there is one, else at a rune boundary. Deterministic in the
// body, so every read of an op renders the same parts.
func SplitArticleBody(body string) []string {
	s := EscapeArticleBody(body)
	var out []string
	for len(s) > SplitChunkMaxBytes {
		cut := SplitChunkMaxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 { // not valid UTF-8 (CheckArticleSplit refuses it): never loop
			cut = SplitChunkMaxBytes
		}
		if nl := strings.LastIndexByte(s[:cut], '\n'); nl >= cut/2 {
			cut = nl + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" || len(out) == 0 {
		out = append(out, s)
	}
	return out
}

// PartMarker is the marker part i of n of op opID carries:
// <!-- hk-op:<op> part:<i>/<n> -->. It is not OpMarker(opID) with a suffix, so
// no marker search for the op finds a part as its index.
func PartMarker(opID int64, i, n int) string {
	return markerPrefix + strconv.FormatInt(opID, 10) + " part:" + strconv.Itoa(i) + "/" + strconv.Itoa(n) + " -->"
}

// SplitPartSummary is part i of n's summary: the document's summary cut first,
// so the suffix " / part i of n" always fits ArticleSummaryLimit and every
// part's summary differs (S4).
func SplitPartSummary(summary string, i, n int) string {
	suffix := " / part " + strconv.Itoa(i) + " of " + strconv.Itoa(n)
	return truncateRunes(summary, ArticleSummaryLimit-utf8.RuneCountInString(suffix)) + suffix
}

// RenderSplitPart is part i of n's content: the provenance line, the chunk (a
// piece of the escaped body) and the part marker.
func RenderSplitPart(actor string, at time.Time, opID int64, i, n int, chunk string) string {
	return ProvenanceLine(actor, at, opID) + "\n\n" + chunk + "\n\n" + PartMarker(opID, i, n)
}

// RenderSplitIndex is the index article's content: in RenderArticleCreate's
// form, a body that lists every part by its summary.
func RenderSplitIndex(actor string, at time.Time, opID int64, summary string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "이 문서는 YouTrack 글 하나의 한도(%d KiB)보다 커서 %d개 부분으로 나뉘었습니다. 각 부분은 이 글의 하위 글입니다.\n", ArticlePartMaxBytes>>10, n)
	for i := 1; i <= n; i++ {
		b.WriteString("\n- " + SplitPartSummary(summary, i, n))
	}
	return RenderArticleCreate(actor, at, opID, b.String())
}

// CheckArticleSplit explains why a long document's split request cannot be
// sent: the article request's own checks (CheckArticle) without its size
// bound, a body that does need a split, and no legacy article (a document the
// legacy sync wrote is never created again).
func CheckArticleSplit(req ArticleRequest) error {
	if !NeedsSplit(req.Body) {
		return fmt.Errorf("%w: document body is within %d bytes: not split", ErrInvalid, MaxArticleBytes)
	}
	if req.LegacyArticleID != "" {
		return fmt.Errorf("%w: a document the legacy sync wrote is not split", ErrInvalid)
	}
	short := req
	short.Body = ""
	if err := CheckArticle(short); err != nil {
		return err
	}
	if !utf8.ValidString(req.Body) || strings.ContainsRune(req.Body, 0) {
		return fmt.Errorf("%w: document body is not valid UTF-8 text without NUL", ErrInvalid)
	}
	return nil
}

// ArticleSplitOp is the op a long document's split stores: one per document
// (key ArticleKey under its own kind), on the document's lane, with
// article_create's payload.
func ArticleSplitOp(req ArticleRequest) (NewOp, error) {
	if req.DocumentID <= 0 {
		return NewOp{}, fmt.Errorf("%w: document id must be positive", ErrInvalid)
	}
	payload, err := json.Marshal(ArticleCreatePayload{DocumentID: req.DocumentID, Summary: req.Summary, Body: req.Body, YTProjectShort: req.YTProjectShort,
		ParentFamily: req.ParentFamily, ParentArticleID: req.ParentArticleID})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.DocumentID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindArticleSplit,
		Key:        ArticleKey(req.DocumentID),
		PayloadSHA: payloadSHA(KindArticleSplit, ArticleEntity, entityID, req.YTProjectID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: ArticleEntity,
		EntityID:   entityID,
		YTTargetID: req.YTProjectID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// splitDoc is one split op rendered: the payload, and the exact summary and
// content of the index and of every part.
type splitDoc struct {
	p             ArticleCreatePayload
	index         string
	partSummaries []string
	partContents  []string
	partMarkers   []string
	parts         int
}

func renderSplit(op Op) (splitDoc, error) {
	p, err := articleCreateOf(op)
	if err != nil {
		return splitDoc{}, err
	}
	if !NeedsSplit(p.Body) || p.LegacyArticleID != "" {
		return splitDoc{}, fmt.Errorf("%w: op %d is not a long document's split", ErrInvalid, op.ID)
	}
	at := op.CreatedAt.UTC().Truncate(time.Second)
	chunks := SplitArticleBody(p.Body)
	n := len(chunks)
	d := splitDoc{p: p, parts: n, index: RenderSplitIndex(op.Actor, at, op.ID, p.Summary, n)}
	for i, c := range chunks {
		d.partSummaries = append(d.partSummaries, SplitPartSummary(p.Summary, i+1, n))
		d.partContents = append(d.partContents, RenderSplitPart(op.Actor, at, op.ID, i+1, n, c))
		d.partMarkers = append(d.partMarkers, PartMarker(op.ID, i+1, n))
	}
	return d, nil
}

// articleSplit is the article_split kind. Its Send and Evidence are never
// called: the protocol sends and recovers it by plan (sendSplit,
// reconcileSplit). Its create holds the family-root logic it shares with
// article_create.
type articleSplit struct {
	create articleCreate
}

// Render is the index's content.
func (articleSplit) Render(op Op) (string, error) {
	d, err := renderSplit(op)
	if err != nil {
		return "", err
	}
	return d.index, nil
}

func (articleSplit) Send(context.Context, Remote, Op, string) (string, error) {
	return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "article_split is sent by plan"}
}

func (articleSplit) Evidence(context.Context, Remote, Op, string) (Evidence, error) {
	return Evidence{}, errors.New("article_split is recovered by plan")
}

// MapRows maps the document to its index article.
func (articleSplit) MapRows(op Op, ytID string) []MapRow {
	return []MapRow{{EntityKind: ArticleEntity, HKKey: op.EntityID, YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}}
}

// HoldError is a read of a split family that shows a state no plan may act
// on: the index gone or not hk's rendering, two articles for one part, a
// part carrying its marker with other content, an index whose create may have
// been sent and that no read finds. Nothing is sent; the op stays
// remote_unknown under Reason for an operator.
type HoldError struct {
	Reason string
	Detail string
}

func (e *HoldError) Error() string { return e.Reason + ": " + e.Detail }

// Reasons a split family is held.
const (
	// ReasonIndexNotFound: a create of the index may have been sent and no
	// read finds the index; a search's absence is not proof (S6), so no
	// second index is created.
	ReasonIndexNotFound = "index_not_found"
	// ReasonIndexGone: the recorded index article is gone (404).
	ReasonIndexGone = "index_gone"
	// ReasonOutcomeAmbiguous: the latest plan's in-flight create answered
	// ambiguously (5xx, 408, timeout) and no read shows it yet; it may still
	// land, so nothing is created beside it.
	ReasonOutcomeAmbiguous = "outcome_ambiguous"
	// ReasonDuplicatePart: two children of the index hold one part.
	ReasonDuplicatePart = "duplicate_part"
)

// familyRead is one read of a split op's family.
type familyRead struct {
	// index is the index article's id, "" when the read found none.
	index string
	// parts[i-1] is the child of the index that holds exactly part i, ""
	// when it has none.
	parts []string
}

func (r familyRead) complete() bool {
	if r.index == "" {
		return false
	}
	for _, id := range r.parts {
		if id == "" {
			return false
		}
	}
	return true
}

func (r familyRead) has(step int) bool {
	if step == 0 {
		return r.index != ""
	}
	return step <= len(r.parts) && r.parts[step-1] != ""
}

// readFamily reads op's family without writing. known is the index id the op
// recorded (or this executor created): it is read by id. Without one, search
// says whether an index create of this op may have been sent: then the index
// is looked up by its marker under its parent (the only search); otherwise no
// index of this op can exist and none is looked for. An index found is read
// directly, then each of its children, and part i is the child under it whose
// summary and content are exactly part i's (S3). Errors from the remote are
// returned as they are; a state no plan may act on is a *HoldError.
func (k articleSplit) readFamily(ctx context.Context, ar ArticleRemote, op Op, d splitDoc, known string, search bool) (familyRead, error) {
	r := familyRead{parts: make([]string, d.parts)}
	var index RemoteArticle
	switch {
	case known != "":
		got, err := ar.ReadArticle(ctx, known)
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return r, &HoldError{Reason: ReasonIndexGone, Detail: "index article " + known + " is gone"}
		}
		if err != nil {
			return r, fmt.Errorf("index %s: %w", known, err)
		}
		if got.ID != known || got.Summary != d.p.Summary || got.Content != d.index {
			return r, &HoldError{Reason: ReasonMarkerMismatch, Detail: "index article " + known + " does not hold the op's rendered index"}
		}
		index = got
	case search:
		parent, found, err := k.create.evidenceParent(ctx, ar, op, d.p)
		if err != nil {
			return r, err
		}
		if !found {
			return r, nil
		}
		hits, err := ar.FindArticles(ctx, ArticleSearch{ProjectShort: d.p.YTProjectShort, ProjectID: op.YTTargetID, Query: d.p.Summary, Summary: d.p.Summary, ParentID: parent})
		if err != nil {
			return r, fmt.Errorf("index of %q: %w", d.p.Summary, err)
		}
		var exact []string
		for _, h := range markedArticles(hits, op.ID) {
			if h.Content != d.index {
				return r, &HoldError{Reason: ReasonMarkerMismatch, Detail: "article " + h.ID + " carries the op's marker with other content"}
			}
			exact = append(exact, h.ID)
		}
		switch {
		case len(exact) > 1:
			return r, &HoldError{Reason: ReasonMultipleMatches, Detail: "articles " + strings.Join(exact, ",") + " carry the op's marker"}
		case len(exact) == 0:
			return r, nil
		}
		// The search returns no children: read the index directly.
		got, err := ar.ReadArticle(ctx, exact[0])
		if err != nil {
			return r, fmt.Errorf("index %s: %w", exact[0], err)
		}
		if got.ID != exact[0] || got.Content != d.index {
			return r, &HoldError{Reason: ReasonMarkerMismatch, Detail: "index article " + exact[0] + " does not hold the op's rendered index"}
		}
		index = got
	default:
		return r, nil
	}
	r.index = index.ID
	for _, child := range index.ChildIDs {
		got, err := ar.ReadArticle(ctx, child)
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			continue // deleted since the index was read: not a part
		}
		if err != nil {
			return r, fmt.Errorf("child %s of index %s: %w", child, index.ID, err)
		}
		if got.ID != child || got.ParentID != index.ID {
			continue
		}
		for i := 0; i < d.parts; i++ {
			if !strings.Contains(got.Content, d.partMarkers[i]) {
				continue
			}
			if got.Content != d.partContents[i] || got.Summary != d.partSummaries[i] {
				// A part carrying its marker with other text (truncated or
				// edited) is never accepted and never adopted as another
				// part; creating one beside it would duplicate it (S3).
				return r, &HoldError{Reason: ReasonMarkerMismatch, Detail: fmt.Sprintf("article %s carries part %d of %d's marker with other content", child, i+1, d.parts)}
			}
			if r.parts[i] != "" {
				return r, &HoldError{Reason: ReasonDuplicatePart, Detail: fmt.Sprintf("articles %s and %s both hold part %d of %d", r.parts[i], child, i+1, d.parts)}
			}
			r.parts[i] = child
		}
	}
	return r, nil
}

// errSplitNoID is a create that answered 2xx without a usable id: it may
// exist, and it is never taken as done (S5).
var errSplitNoID = errors.New("2xx reply without a valid id")

// executeSplit sends plan's creates in order, calling mark(i, index) before
// step i's request with the index id known at that point ("" only before the
// index create). It returns how many creates completed and the index id.
func (articleSplit) executeSplit(ctx context.Context, ar ArticleRemote, op Op, d splitDoc, parent string, plan SplitPlan, mark func(step int, index string) error) (n int, index string, err error) {
	index = plan.Index
	for i, s := range plan.Steps {
		if err := mark(i, index); err != nil {
			return i, index, err
		}
		in := ArticleCreate{ProjectID: op.YTTargetID, ParentID: index}
		if s == 0 {
			in.ParentID, in.Summary, in.Content = parent, d.p.Summary, d.index
		} else {
			in.Summary, in.Content = d.partSummaries[s-1], d.partContents[s-1]
		}
		id, err := ar.CreateArticle(ctx, in)
		if err != nil {
			return i, index, err
		}
		if !resultIDRE.MatchString(id) {
			return i, index, errSplitNoID
		}
		if s == 0 {
			index = id
		}
	}
	return len(plan.Steps), index, nil
}
