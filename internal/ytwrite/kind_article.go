package ytwrite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The document kinds (hk 4206, docs/youtrack-write-protocol.md "Kinds
// article_create and article_update"): after the import ended, an hk
// document reaches its YouTrack article only through these. article_update
// writes a bound document's summary and body to its article
// (yt_article_bindings); article_create makes the article of a document
// created after the import, at most once per document (key ArticleKey), or
// adopts the one article that already carries the op's marker. Both ride the
// document's lane ("document", decimal documents id). A document whose body
// is over MaxArticleBytes (the import would split it) is not written (D4).

func init() {
	registerKind(KindArticleCreate, func(Config) Kind { return articleCreate{} })
	registerKind(KindArticleUpdate, func(Config) Kind { return articleUpdate{} })
}

const (
	// KindArticleCreate creates (or adopts) a document's article.
	KindArticleCreate = "article_create"
	// KindArticleUpdate writes a document's summary and body to its bound
	// article.
	KindArticleUpdate = "article_update"
	// ArticleEntity is the lane entity kind of the document ops.
	ArticleEntity = "document"
)

// MaxArticleBytes is the import's SAFE_PART_BYTES (youtrack/import/client.js):
// a body longer than this many UTF-8 bytes was imported as an index and
// parts, which the document kinds do not write.
const MaxArticleBytes = 240 * 1024

// ArticleSummaryLimit is the import's summary cut, in code points.
const ArticleSummaryLimit = 250

// ReasonLegacySyncedUnverified: the document has an article the legacy
// `youtrack sync` made (youtrack_doc_articles) that the create could not
// verify as its own (gone, not in the legacy project, without the document's
// legacy marker, the legacy project unknown, or split into parts). Nothing is created: never a
// second article (handoffkeep-design, hk 4206).
const ReasonLegacySyncedUnverified = "legacy_synced_unverified"

// LegacyDocToken is the legacy `youtrack sync` search word for a document
// key (internal/youtrack docs.go): "hkdoc" and the first 12 bytes of the
// key's sha256, in hex.
func LegacyDocToken(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "hkdoc" + hex.EncodeToString(sum[:12])
}

// LegacyDocMarker is the line the legacy sync put at the top of a document's
// article: <!-- hk-marker:<LegacyDocToken(key)> -->.
func LegacyDocMarker(key string) string { return "<!-- hk-marker:" + LegacyDocToken(key) + " -->" }

// ArticleKey is the idempotency key of a document's article_create op: one
// create per document.
func ArticleKey(documentID int64) string {
	return "document-article:" + strconv.FormatInt(documentID, 10)
}

// ArticleRetryKey is the key of the create that replaces the document's
// article_create op rejectedOpID, which was rejected invalid_issue before it
// was ever claimed (hk 4295: a body quoting the op marker, which the render
// now escapes). Such an op made no article, so the retry is still the
// document's only create; deriving the key from the rejected op keeps the
// retry idempotent.
func ArticleRetryKey(documentID, rejectedOpID int64) string {
	return ArticleKey(documentID) + ":retry-" + strconv.FormatInt(rejectedOpID, 10)
}

// escapedMarkerPrefix is markerPrefix with a zero-width space after its "<":
// YouTrack shows it as the same text, in prose and in code alike (an HTML
// entity would show as "&lt;" inside a code span), and no marker scan
// matches it.
const escapedMarkerPrefix = "<\u200b!-- hk-op:"

// EscapeArticleBody is a document body as an article carries it: every
// quoted op marker prefix escaped (hk 4295), so the op's own marker stays
// the only real one in the content. A body without the prefix is returned
// unchanged. The escape is one-way: a body may already hold the escaped
// form, so a reader compares EscapeArticleBody of the document with the
// article and never unescapes.
func EscapeArticleBody(body string) string {
	return strings.ReplaceAll(body, markerPrefix, escapedMarkerPrefix)
}

// OpMarkerPrefix is the reserved prefix of every hk op marker, for a reader
// that must find a quoted one in SQL.
const OpMarkerPrefix = markerPrefix

// HasOpMarker reports whether text quotes the reserved op marker prefix.
func HasOpMarker(text string) bool { return strings.Contains(text, markerPrefix) }

// ContentSHA is the sha256 of an article's content, lower-case hex: the
// value article_update's evidence compares and yt_article_bindings keeps.
func ContentSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// checkArticleText explains why a summary and body cannot be written: a
// summary of 1 to ArticleSummaryLimit code points on one line, and a body of
// valid UTF-8 without NUL (jsonb cannot hold it) within MaxArticleBytes. An
// empty body is an empty article, as the import wrote it.
func checkArticleText(summary, body string) error {
	if summary == "" || strings.ContainsAny(summary, "\r\n") || !utf8.ValidString(summary) || strings.ContainsRune(summary, 0) ||
		utf8.RuneCountInString(summary) > ArticleSummaryLimit {
		return fmt.Errorf("%w: article summary must be one line of 1-%d code points", ErrInvalid, ArticleSummaryLimit)
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return fmt.Errorf("%w: document body is not valid UTF-8 text without NUL", ErrInvalid)
	}
	if len(body) > MaxArticleBytes {
		return fmt.Errorf("%w: document body is over %d bytes", ErrInvalid, MaxArticleBytes)
	}
	return nil
}

// ArticleRequest is one document whose article hk creates.
type ArticleRequest struct {
	Principal string
	// Actor is who the provenance line names.
	Actor string
	// YTProjectID and YTProjectShort are the article's project; Tenant the
	// YouTrack host.
	YTProjectID    string
	YTProjectShort string
	Tenant         string
	DocumentID     int64
	// Summary is the import's "[<prefix>] <key> [<kind>]" (cut to 250 code
	// points); Body the document's body.
	Summary string
	Body    string
	// LegacyArticleID is the article the legacy `youtrack sync` recorded for
	// the document ("" for none), LegacyProjectID the YouTrack project the
	// legacy sync put it in (its youtrack_projects mapping) and LegacyMarker
	// its legacy marker (LegacyDocMarker of the key): Send adopts that
	// article when it verifies and otherwise creates nothing.
	LegacyArticleID string
	LegacyProjectID string
	LegacyMarker    string
	// RetryOf is the document's create op this one replaces (0 for the
	// first create): see ArticleRetryKey.
	RetryOf int64
}

// ArticleCreatePayload is the immutable payload of an article_create op: the
// summary and body as they were when the op was prepared, and the project's
// short name (the search scope). The document id is the op's EntityID and
// the project id its YTTargetID.
type ArticleCreatePayload struct {
	DocumentID     int64  `json:"document_id"`
	Summary        string `json:"summary"`
	Body           string `json:"body"`
	YTProjectShort string `json:"yt_project_short"`
	// LegacyArticleID, LegacyProjectID and LegacyMarker are
	// ArticleRequest's.
	LegacyArticleID string `json:"legacy_article_id,omitempty"`
	LegacyProjectID string `json:"legacy_project_id,omitempty"`
	LegacyMarker    string `json:"legacy_marker,omitempty"`
}

// CheckArticle explains why an article request cannot be sent. A store that
// must record such a document builds the op with ArticleOp and rejects it in
// the same transaction. A body that quotes the op marker is sent escaped
// (EscapeArticleBody), so the op's own marker stays the create's only
// evidence.
func CheckArticle(req ArticleRequest) error {
	if !principalRE.MatchString(req.Principal) {
		return fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return err
	}
	if req.DocumentID <= 0 {
		return fmt.Errorf("%w: document id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.YTProjectID) || !issueIDRE.MatchString(req.YTProjectShort) {
		return fmt.Errorf("%w: youtrack project id or short name is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	return checkArticleText(req.Summary, req.Body)
}

// ArticleOp is the op an article request stores. Like DecisionOp it does not
// validate the request beyond what it needs to build the op.
func ArticleOp(req ArticleRequest) (NewOp, error) {
	if req.DocumentID <= 0 {
		return NewOp{}, fmt.Errorf("%w: document id must be positive", ErrInvalid)
	}
	payload, err := json.Marshal(ArticleCreatePayload{DocumentID: req.DocumentID, Summary: req.Summary, Body: req.Body, YTProjectShort: req.YTProjectShort,
		LegacyArticleID: req.LegacyArticleID, LegacyProjectID: req.LegacyProjectID, LegacyMarker: req.LegacyMarker})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.DocumentID, 10)
	key := ArticleKey(req.DocumentID)
	if req.RetryOf > 0 {
		key = ArticleRetryKey(req.DocumentID, req.RetryOf)
	}
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindArticleCreate,
		Key:        key,
		PayloadSHA: payloadSHA(KindArticleCreate, ArticleEntity, entityID, req.YTProjectID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: ArticleEntity,
		EntityID:   entityID,
		YTTargetID: req.YTProjectID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// ArticleUpdateRequest is one document's summary and body written to its
// bound article.
type ArticleUpdateRequest struct {
	Principal string
	Actor     string
	// Tenant and ArticleID are the document's binding.
	Tenant     string
	ArticleID  string
	DocumentID int64
	Summary    string
	// Content is the document's body.
	Content string
	// Provenance writes the body in hk's format (RenderArticleCreate, with
	// the update op's own provenance line and marker): set for an article
	// hk created (binding origin op_applied). An imported article keeps the
	// raw body, as the import wrote it.
	Provenance bool
}

// ArticleUpdatePayload is the immutable payload of an article_update op: the
// summary, the body, and whether the body is written in hk's format.
type ArticleUpdatePayload struct {
	Summary    string `json:"summary"`
	Content    string `json:"content"`
	Provenance bool   `json:"provenance,omitempty"`
}

// ArticleUpdateOp is the op an update request stores, with no Key: the
// store's snapshot helper sets it from PayloadSHA, which covers the summary,
// the content, the target article and the tenant.
func ArticleUpdateOp(req ArticleUpdateRequest) (NewOp, error) {
	if !principalRE.MatchString(req.Principal) {
		return NewOp{}, fmt.Errorf("%w: principal is empty or malformed", ErrInvalid)
	}
	if err := checkActor(req.Actor); err != nil {
		return NewOp{}, err
	}
	if req.DocumentID <= 0 {
		return NewOp{}, fmt.Errorf("%w: document id must be positive", ErrInvalid)
	}
	if !issueIDRE.MatchString(req.ArticleID) {
		return NewOp{}, fmt.Errorf("%w: youtrack article id is empty or malformed", ErrInvalid)
	}
	if req.Tenant == "" {
		return NewOp{}, fmt.Errorf("%w: tenant is empty", ErrInvalid)
	}
	if err := checkArticleText(req.Summary, req.Content); err != nil {
		return NewOp{}, err
	}
	payload, err := json.Marshal(ArticleUpdatePayload{Summary: req.Summary, Content: req.Content, Provenance: req.Provenance})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.DocumentID, 10)
	return NewOp{
		Principal:  req.Principal,
		Kind:       KindArticleUpdate,
		PayloadSHA: payloadSHA(KindArticleUpdate, ArticleEntity, entityID, req.ArticleID, req.Tenant, req.Actor, string(payload)),
		Payload:    payload,
		EntityKind: ArticleEntity,
		EntityID:   entityID,
		YTTargetID: req.ArticleID,
		Tenant:     req.Tenant,
		Actor:      req.Actor,
	}, nil
}

// RemoteArticle is one article as the remote reads it.
type RemoteArticle struct {
	ID        string
	ProjectID string
	Summary   string
	Content   string
	// Child is set on an article with a parent (a split document's part).
	Child bool
}

// ArticleCreate is one article create: the project and the text.
type ArticleCreate struct {
	ProjectID string
	Summary   string
	Content   string
}

// ArticleRemote is the YouTrack surface of the document kinds. A Remote that
// does not implement it cannot send them.
type ArticleRemote interface {
	// FindArticles returns every top-level article of the project (short
	// name and id) whose summary is exactly summary, with its content, or
	// an error. A result it cannot prove complete is an error; a 429 is a
	// *RemoteError with the server's RetryAfter.
	FindArticles(ctx context.Context, projectShort, projectID, summary string) ([]RemoteArticle, error)
	// CreateArticle makes exactly one POST and returns the new article's
	// id. Its errors follow CreateComment's.
	CreateArticle(ctx context.Context, in ArticleCreate) (id string, err error)
	// UpdateArticle sets the article's summary and content in exactly one
	// POST and returns its id. Its errors follow CreateComment's.
	UpdateArticle(ctx context.Context, articleID, summary, content string) (id string, err error)
	// ReadArticle reads one article by id, with its content. A 429 is a
	// *RemoteError with the server's RetryAfter.
	ReadArticle(ctx context.Context, articleID string) (RemoteArticle, error)
}

// articleCreate is the article_create kind. Its text (Render) is the
// article's content, which carries the provenance line and the marker; Send
// takes the summary from the same op.
type articleCreate struct{}

func articleCreateOf(op Op) (ArticleCreatePayload, error) {
	var p ArticleCreatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return p, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	id, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || id <= 0 || op.EntityKind != ArticleEntity || p.DocumentID != id {
		return p, fmt.Errorf("%w: op %d is not on its document's lane", ErrInvalid, op.ID)
	}
	return p, nil
}

// RenderArticleCreate is the content an article_create op writes, the body
// escaped (EscapeArticleBody):
//
//	작성: <actor> · <UTC> · hk op <op>
//
//	<document body>
//
//	<!-- hk-op:<op> -->
func RenderArticleCreate(actor string, at time.Time, opID int64, body string) string {
	return ProvenanceLine(actor, at, opID) + "\n\n" + EscapeArticleBody(body) + "\n\n" + OpMarker(opID)
}

// ParseArticleContent reads an article content in RenderArticleCreate's
// form back: the op named by the provenance line, whose marker ends the
// content, and the body between them as the article carries it (escaped).
// ok is false for any other text, including one whose body holds a marker
// prefix. Like RenderedOpID, the form alone is not ownership.
func ParseArticleContent(content string) (opID int64, body string, ok bool) {
	first, rest, found := strings.Cut(content, "\n\n")
	if !found {
		return 0, "", false
	}
	actor, _, id, ok := ParseProvenance(first)
	if !ok || checkActor(actor) != nil {
		return 0, "", false
	}
	body, found = strings.CutSuffix(rest, "\n\n"+OpMarker(id))
	if !found || HasOpMarker(body) {
		return 0, "", false
	}
	return id, body, true
}

func (articleCreate) Render(op Op) (string, error) {
	p, err := articleCreateOf(op)
	if err != nil {
		return "", err
	}
	if err := checkArticleText(p.Summary, p.Body); err != nil {
		return "", err
	}
	return RenderArticleCreate(op.Actor, op.CreatedAt.UTC().Truncate(time.Second), op.ID, p.Body), nil
}

// markedArticles are the articles of hits whose content carries op's marker.
func markedArticles(hits []RemoteArticle, opID int64) []RemoteArticle {
	marker := OpMarker(opID)
	var out []RemoteArticle
	for _, h := range hits {
		if strings.Contains(h.Content, marker) {
			out = append(out, h)
		}
	}
	return out
}

// Send looks for the document's article and writes only when there is none.
// A document the legacy sync wrote (LegacyArticleID) is never created: its
// article is read and adopted (no POST) when it is that article, in the
// legacy project (LegacyProjectID), carrying the document's legacy marker; it
// may be a child (the legacy sync put every document under its prefix's index
// article). Otherwise the op is rejected legacy_synced_unverified (a 404
// included; another failed read is not_sent). Else exactly one article of the project with the op's
// summary that carries the op's marker is adopted (no POST); two or more are
// ambiguous_issue (no POST); none is exactly one create with the summary and
// the content.
func (articleCreate) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	ar, ok := remote.(ArticleRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot create articles"}
	}
	p, err := articleCreateOf(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	if p.LegacyArticleID != "" {
		return "", legacyArticle(ctx, ar, op, p)
	}
	hits, err := ar.FindArticles(ctx, p.YTProjectShort, op.YTTargetID, p.Summary)
	if err != nil {
		return "", &NotSentError{Err: err}
	}
	switch marked := markedArticles(hits, op.ID); {
	case len(marked) == 1:
		return "", &AdoptedError{ID: marked[0].ID}
	case len(marked) > 1:
		ids := make([]string, 0, len(marked))
		for _, h := range marked {
			ids = append(ids, h.ID)
		}
		return "", &RefusedError{Reason: ReasonAmbiguousIssue, Detail: fmt.Sprintf("project %s has %d articles carrying op %d's marker: %s", op.YTTargetID, len(marked), op.ID, strings.Join(ids, ","))}
	}
	return ar.CreateArticle(ctx, ArticleCreate{ProjectID: op.YTTargetID, Summary: p.Summary, Content: text})
}

// legacyArticle is Send for a document the legacy sync wrote: an
// *AdoptedError for its verified article, a *RefusedError
// (ReasonLegacySyncedUnverified) for one that does not verify, or a
// *NotSentError for a read that failed otherwise.
func legacyArticle(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) error {
	a, err := ar.ReadArticle(ctx, p.LegacyArticleID)
	var re *RemoteError
	switch {
	case errors.As(err, &re) && re.Status == http.StatusNotFound:
		return &RefusedError{Reason: ReasonLegacySyncedUnverified, Detail: "legacy article " + p.LegacyArticleID + " is gone"}
	case err != nil:
		return &NotSentError{Err: err}
	case a.ID != p.LegacyArticleID || p.LegacyProjectID == "" || a.ProjectID != p.LegacyProjectID || p.LegacyMarker == "" || !strings.Contains(a.Content, p.LegacyMarker):
		return &RefusedError{Reason: ReasonLegacySyncedUnverified, Detail: fmt.Sprintf("legacy article %s (project %s) is not an article of the legacy project %q carrying the document's legacy marker",
			p.LegacyArticleID, a.ProjectID, p.LegacyProjectID)}
	}
	return &AdoptedError{ID: a.ID}
}

// Evidence lists the project's articles with the op's summary. One that
// carries the op's marker and whose content is exactly the rendered text is
// a match; one that carries the marker with other content is a mismatch, so
// the shared rule keeps the op remote_unknown.
func (articleCreate) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	ar, ok := remote.(ArticleRemote)
	if !ok {
		return ev, errors.New("the remote cannot list articles")
	}
	p, err := articleCreateOf(op)
	if err != nil {
		return ev, err
	}
	hits, err := ar.FindArticles(ctx, p.YTProjectShort, op.YTTargetID, p.Summary)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	for _, h := range markedArticles(hits, op.ID) {
		if h.Content == text {
			ev.Matches = append(ev.Matches, h.ID)
		} else {
			ev.Mismatched++
		}
	}
	return ev, nil
}

// needsProvenance: an adopted article's content is not what the op wrote, so
// its binding starts with needs_update; an operator's resolution must say
// which it is.
func (articleCreate) needsProvenance() bool { return true }

// MapRows maps the document to its article, with the project as parent.
func (articleCreate) MapRows(op Op, ytID string) []MapRow {
	return []MapRow{{EntityKind: ArticleEntity, HKKey: op.EntityID, YTID: ytID, YTParentID: op.YTTargetID, OpID: op.ID}}
}

// articleUpdate is the article_update kind: the whole summary and content
// written to the bound article. Its text (Render) is the content: the body,
// or for a Provenance op the body in RenderArticleCreate's form with this
// op's provenance line and marker.
type articleUpdate struct{}

func articleUpdateOf(op Op) (ArticleUpdatePayload, error) {
	var p ArticleUpdatePayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return p, fmt.Errorf("%w: payload of op %d: %v", ErrInvalid, op.ID, err)
	}
	if op.EntityKind != ArticleEntity || !issueIDRE.MatchString(op.YTTargetID) || p.Summary == "" {
		return p, fmt.Errorf("%w: op %d is not a document article update", ErrInvalid, op.ID)
	}
	return p, nil
}

func (articleUpdate) Render(op Op) (string, error) {
	p, err := articleUpdateOf(op)
	if err != nil {
		return "", err
	}
	if !p.Provenance {
		return EscapeArticleBody(p.Content), nil
	}
	return RenderArticleCreate(op.Actor, op.CreatedAt.UTC().Truncate(time.Second), op.ID, p.Content), nil
}

func (articleUpdate) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	ar, ok := remote.(ArticleRemote)
	if !ok {
		return "", &RefusedError{Reason: ReasonUnsupportedKind, Detail: "the remote cannot update articles"}
	}
	p, err := articleUpdateOf(op)
	if err != nil {
		return "", &RefusedError{Reason: ReasonInvalidIssue, Detail: err.Error()}
	}
	return ar.UpdateArticle(ctx, op.YTTargetID, p.Summary, text)
}

// Evidence reads the article: when its summary is the op's and its content's
// sha256 is the rendered text's, the article is the one match; anything else
// is one mismatch, which the shared rule keeps remote_unknown.
func (articleUpdate) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	ar, ok := remote.(ArticleRemote)
	if !ok {
		return ev, errors.New("the remote cannot read articles")
	}
	p, err := articleUpdateOf(op)
	if err != nil {
		return ev, err
	}
	got, err := ar.ReadArticle(ctx, op.YTTargetID)
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	if got.Summary == p.Summary && ContentSHA(got.Content) == ContentSHA(text) {
		ev.Matches = []string{op.YTTargetID}
	} else {
		ev.Mismatched = 1
	}
	return ev, nil
}

// MapRows: an update creates nothing to map.
func (articleUpdate) MapRows(Op, string) []MapRow { return nil }
