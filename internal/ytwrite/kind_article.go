package ytwrite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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
	registerKind(KindArticleCreate, func(cfg Config) Kind { return articleCreate{ledger: cfg.Ledger} })
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
const MaxArticleBytes = ArticlePartMaxBytes

// ArticleSummaryLimit is the import's summary cut, in code points.
const ArticleSummaryLimit = SummaryMaxChars

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
	// ParentFamily is the document key's first segment: the article goes
	// under its family's root article, which is created when missing.
	// ParentArticleID is the article of the policy document an amending
	// decision names: the article goes under it instead. Set at most one.
	ParentFamily    string
	ParentArticleID string
	// RetryOf is the document's create op this one replaces (0 for the
	// first create): see ArticleRetryKey. RetryKey is that op's key; empty
	// means the first create's (ArticleKey), and a retry of a retry
	// (hk 4357) gives its own.
	RetryOf  int64
	RetryKey string
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
	// ParentFamily and ParentArticleID are ArticleRequest's.
	ParentFamily    string `json:"parent_family,omitempty"`
	ParentArticleID string `json:"parent_article_id,omitempty"`
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
	if req.ParentFamily != "" && !familyRE.MatchString(req.ParentFamily) {
		return fmt.Errorf("%w: parent family %q is malformed", ErrInvalid, req.ParentFamily)
	}
	if req.ParentArticleID != "" && !issueIDRE.MatchString(req.ParentArticleID) {
		return fmt.Errorf("%w: parent article id is malformed", ErrInvalid)
	}
	if req.ParentFamily != "" && req.ParentArticleID != "" {
		return fmt.Errorf("%w: an article has one parent: a family or an article, not both", ErrInvalid)
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
		LegacyArticleID: req.LegacyArticleID, LegacyProjectID: req.LegacyProjectID, LegacyMarker: req.LegacyMarker,
		ParentFamily: req.ParentFamily, ParentArticleID: req.ParentArticleID})
	if err != nil {
		return NewOp{}, err
	}
	entityID := strconv.FormatInt(req.DocumentID, 10)
	key := ArticleKey(req.DocumentID)
	if req.RetryOf > 0 {
		key = ArticleRetryKey(req.DocumentID, req.RetryOf)
		if req.RetryKey != "" {
			key = req.RetryKey + ":retry-" + strconv.FormatInt(req.RetryOf, 10)
		}
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
	// ParentID is the article's parent, "" for a top-level article (a
	// split document's part is a child of its index article).
	ParentID string
	// CreatedAt is when the article was created, zero when the remote did not
	// say. A family with several roots uses its oldest by this (hk 4797).
	CreatedAt time.Time
	// ChildIDs are the article's sub-articles, as ReadArticle reads them
	// directly (childArticles), never by search; nil from FindArticles. A
	// split document's parts are found here (hk 5085).
	ChildIDs []string
}

// ArticleCreate is one article create: the project and the text.
type ArticleCreate struct {
	ProjectID string
	// ParentID, when set, makes the article a child of that article.
	ParentID string
	Summary  string
	Content  string
}

// ArticleRemote is the YouTrack surface of the document kinds. A Remote that
// does not implement it cannot send them.
type ArticleRemote interface {
	// FindArticles returns the project's articles whose summary or content
	// holds the search's Query, whose summary is exactly the search's
	// Summary, and whose parent is its ParentID, with their content; or an
	// error. A result it cannot prove complete is an error; a 429 is a
	// *RemoteError with the server's RetryAfter.
	FindArticles(ctx context.Context, s ArticleSearch) ([]RemoteArticle, error)
	// CreateArticle makes exactly one POST and returns the new article's
	// id. Its errors follow CreateComment's.
	CreateArticle(ctx context.Context, in ArticleCreate) (id string, err error)
	// UpdateArticle sets the article's summary and content in exactly one
	// POST and returns its id. Its errors follow CreateComment's.
	UpdateArticle(ctx context.Context, articleID, summary, content string) (id string, err error)
	// ReadArticle reads one article by id, with its content, its parent and
	// its children's ids (ChildIDs). A 429 is a *RemoteError with the
	// server's RetryAfter.
	ReadArticle(ctx context.Context, articleID string) (RemoteArticle, error)
}

// ArticleSearch is one article lookup: the phrase searched, and what a hit
// must be. Query is what keeps a search narrow: a document's own summary finds
// its article, and a family root's own text finds the root, where the bare
// family word matches every article of the family that mentions it (hk 4784).
type ArticleSearch struct {
	// ProjectShort is the short name the search is scoped to; ProjectID the
	// project a hit must be in.
	ProjectShort string
	ProjectID    string
	// Query is the phrase searched in the articles' summary and content.
	Query string
	// Summary keeps only the hits whose summary is exactly this.
	Summary string
	// ParentID keeps only the children of that article; "" the top-level
	// articles.
	ParentID string
}

// articleCreate is the article_create kind. Its text (Render) is the
// article's content, which carries the provenance line and the marker; Send
// takes the summary from the same op. Its ledger holds the record of each
// family's root and the family lane that serialises root creation (hk 4797);
// a Protocol built without New (the operator's resolve) has none, and Send
// then finds the root by its marker without recording it.
type articleCreate struct {
	ledger Ledger
}

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

// familyRE is a family name, the first segment of a document key. It becomes
// the family root's summary and sits in the root's marker, so it stays plain.
var familyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidFamily reports whether family can name a family root (a document
// key's first segment, see FamilyRootMarker).
func ValidFamily(family string) bool { return familyRE.MatchString(family) }

// FamilyRootMarker is the marker a family root article carries. The root is
// the parent of every document of its family.
func FamilyRootMarker(family string) string {
	return "<!-- hk-family:" + family + " -->"
}

// FamilyRootPhrase is the sentence a family root carries, without its final
// period: the phrase its lookup searches for. A root is found by its own text,
// never by the bare family word, which every document of the family also
// carries (hk 4784: a word search for a family filled a 100-hit page in a
// project of 1 200 documents, so the search could never prove completeness and
// the create never sent).
func FamilyRootPhrase(family string) string {
	return "Family root for " + family + " documents"
}

// FamilyRootContent is a family root article's content: FamilyRootPhrase and
// the marker.
func FamilyRootContent(family string) string {
	return FamilyRootPhrase(family) + ".\n\n" + FamilyRootMarker(family)
}

// familyRoots lists the project's roots of one family: the articles whose
// summary is the family and whose content carries the family marker. Both
// queries are narrow — neither is the bare family word, which every document of
// the family also carries (hk 4784) — and it asks both, because neither alone
// sees every root: the sentence finds the roots hk wrote, and the marker finds
// one an operator edited (its sentence gone, the marker kept), the marker being
// the root's identity. So no marker-carrying root is missed and no create makes
// a second one beside it (review of PR #441). A search that cannot prove
// completeness is an error, never a create.
func familyRoots(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) ([]RemoteArticle, error) {
	marker := FamilyRootMarker(p.ParentFamily)
	var roots []RemoteArticle
	for _, query := range []string{FamilyRootPhrase(p.ParentFamily), marker} {
		hits, err := ar.FindArticles(ctx, ArticleSearch{ProjectShort: p.YTProjectShort, ProjectID: op.YTTargetID, Query: query, Summary: p.ParentFamily})
		if err != nil {
			return nil, fmt.Errorf("family root %q: %w", p.ParentFamily, err)
		}
		for _, h := range hits {
			if strings.Contains(h.Content, marker) && !slices.ContainsFunc(roots, func(r RemoteArticle) bool { return r.ID == h.ID }) {
				roots = append(roots, h)
			}
		}
	}
	return roots, nil
}

// OldestFamilyRoot is the root a family uses when its project holds several
// articles carrying its marker — a retry an earlier release made, or a copy a
// person made. It is the oldest by creation time, then by article id (hk
// 4797), which is the one every document of the family goes under; nothing is
// created beside it. found is false for no roots.
func OldestFamilyRoot(roots []RemoteArticle) (root RemoteArticle, found bool) {
	if len(roots) == 0 {
		return RemoteArticle{}, false
	}
	best := roots[0]
	for _, r := range roots[1:] {
		if familyRootOlder(r, best) {
			best = r
		}
	}
	return best, true
}

// familyRootOlder orders two roots: an older creation time wins, and a root
// whose creation time the remote did not report sorts after one it did; the id
// breaks a tie. The order is total, so the choice does not depend on the order
// the search returned the roots in.
func familyRootOlder(a, b RemoteArticle) bool {
	switch {
	case a.CreatedAt.IsZero() != b.CreatedAt.IsZero():
		return b.CreatedAt.IsZero()
	case !a.CreatedAt.Equal(b.CreatedAt):
		return a.CreatedAt.Before(b.CreatedAt)
	default:
		return a.ID < b.ID
	}
}

// articleParent is the article a document's article goes under: the amended
// policy article when the payload names one, else the family root, else ""
// for a payload that names neither. It never writes: a missing root is found
// false, so Evidence measures only what is there, and when several roots carry
// the family's marker the oldest one is the family's (hk 4797).
func articleParent(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (parent string, found bool, err error) {
	if p.ParentArticleID != "" {
		return p.ParentArticleID, true, nil
	}
	if p.ParentFamily == "" {
		return "", true, nil
	}
	roots, err := familyRoots(ctx, ar, op, p)
	if err != nil {
		return "", false, err
	}
	root, ok := OldestFamilyRoot(roots)
	if !ok {
		return "", false, nil
	}
	return root.ID, true, nil
}

// familyRoot is Send's parent for a document whose payload names a family: the
// one root article every document of the family goes under. A root the store
// has recorded is used without a search; one it has not is looked up by its
// marker and, when the project holds several, the oldest is the family's root.
// A root is created only when the lookup found none, and the store's family
// lane (Ledger.LockFamily) covers that lookup and create, so two documents of
// a new family cannot make two roots (hk 4797).
func (a articleCreate) familyRoot(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, error) {
	if id, ok, err := a.recordedRoot(ctx, ar, op, p); err != nil || ok {
		return id, err
	}
	if a.ledger == nil {
		return a.searchOrCreateRoot(ctx, ar, op, p)
	}
	unlock, ok, err := a.ledger.LockFamily(ctx, op.Tenant, op.YTTargetID, p.ParentFamily)
	if err != nil {
		return "", &NotSentError{Err: fmt.Errorf("family root %q: %w", p.ParentFamily, err)}
	}
	if !ok {
		return "", &NotSentError{Err: fmt.Errorf("family root %q: another op holds the family's lane", p.ParentFamily)}
	}
	defer unlock()
	// The lane may have changed hands since the first read: the op that held it
	// may have recorded the root the family now uses.
	if id, ok, err := a.recordedRoot(ctx, ar, op, p); err != nil || ok {
		return id, err
	}
	return a.searchOrCreateRoot(ctx, ar, op, p)
}

// searchOrCreateRoot finds the family's root and creates it only when the
// project has none.
func (a articleCreate) searchOrCreateRoot(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, error) {
	parent, found, err := articleParent(ctx, ar, op, p)
	if err != nil {
		return "", &NotSentError{Err: err}
	}
	if found {
		return a.recordedRootID(ctx, op, p, parent)
	}
	return a.createRoot(ctx, ar, op, p)
}

// recordedRoot is the root the store recorded for the family, found false when
// it recorded none or the article is gone: a deleted root is looked up again
// instead of failing every document of the family on a bad parent.
func (a articleCreate) recordedRoot(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, bool, error) {
	if a.ledger == nil {
		return "", false, nil
	}
	id, found, err := a.ledger.FamilyRoot(ctx, op.Tenant, op.YTTargetID, p.ParentFamily)
	if err != nil || !found {
		return "", false, err
	}
	got, err := ar.ReadArticle(ctx, id)
	var re *RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, &NotSentError{Err: fmt.Errorf("family root %q: %w", p.ParentFamily, err)}
	}
	if got.ID != id {
		return "", false, nil
	}
	return id, true, nil
}

// recordedRootID remembers the root, so the family's later documents need no
// search and cannot race a create. A record that cannot be written leaves the
// op unsent: the next attempt finds the same root by its marker and records it,
// and nothing was written to YouTrack in between.
func (a articleCreate) recordedRootID(ctx context.Context, op Op, p ArticleCreatePayload, rootID string) (string, error) {
	if a.ledger == nil {
		return rootID, nil
	}
	if err := a.ledger.RecordFamilyRoot(ctx, op.Tenant, op.YTTargetID, p.ParentFamily, rootID, op.ID); err != nil {
		return "", &NotSentError{Err: fmt.Errorf("family root %q: record: %w", p.ParentFamily, err)}
	}
	return rootID, nil
}

// familyRootReconcileTimeout bounds the one search that settles a family root
// POST whose outcome is unknown, and the record it writes. It runs on a
// context of its own: a POST that timed out may have spent the whole dispatch
// budget, and this search is what turns that uncertainty into the root the POST
// made instead of leaving the document blocked behind it (hk 4797). The
// protocol bounds its own settlement the same way (SettleTimeout).
const familyRootReconcileTimeout = 15 * time.Second

// createRoot makes the family's one root article. A root is not an op, so no op
// row records it; the store's family record does. A POST that may have reached
// YouTrack is never repeated: one search reconciles it, and the root it made is
// the family's. No root after an uncertain POST keeps the failure, which the
// protocol records remote_unknown (hk 4797: that outcome once became a second
// root).
func (a articleCreate) createRoot(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, error) {
	id, err := ar.CreateArticle(ctx, ArticleCreate{ProjectID: op.YTTargetID, Summary: p.ParentFamily, Content: FamilyRootContent(p.ParentFamily)})
	if err == nil && id != "" {
		return a.recordedRootID(ctx, op, p, id)
	}
	if err != nil && beforeSend(err) {
		// The request never left: nothing can exist to reconcile.
		return "", fmt.Errorf("family root %q: %w", p.ParentFamily, err)
	}
	// The POST may have created the root: a lost reply, a timeout, a 5xx or a
	// 2xx without an id. The search is the only evidence there is, and finding
	// a root means the create happened (or another op's did, which is the
	// family's root just the same).
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), familyRootReconcileTimeout)
	defer cancel()
	roots, rerr := familyRoots(rctx, ar, op, p)
	if rerr != nil {
		return "", fmt.Errorf("family root %q: create failed and its reconcile could not search: %w", p.ParentFamily, rerr)
	}
	root, found := OldestFamilyRoot(roots)
	if !found {
		if err != nil {
			return "", fmt.Errorf("family root %q: %w", p.ParentFamily, err)
		}
		return "", fmt.Errorf("family root %q: the create returned no id and no article carries the family marker", p.ParentFamily)
	}
	return a.recordedRootID(rctx, op, p, root.ID)
}

// readFailure is Evidence's error: a 429 keeps its RetryAfter for the shared rule.
func readFailure(ev Evidence, err error) (Evidence, error) {
	var re *RemoteError
	if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
		ev.RetryAfter = re.RetryAfter
	}
	return ev, err
}

// Send looks for the document's article under its parent and writes only
// when there is none. A document the legacy sync wrote (LegacyArticleID) is
// never created: its article is read and adopted (no POST) when it is that
// article, in the legacy project (LegacyProjectID), carrying the document's
// legacy marker; it may be a child (the legacy sync put every document under
// its prefix's index article). Otherwise the op is rejected
// legacy_synced_unverified (a 404 included; another failed read is
// not_sent). Else the parent is the amended policy article or the family
// root (familyRoot: the root the store recorded, else the project's oldest
// one carrying the family marker, else one created — never a second beside an
// existing root), and exactly one article under it with the op's summary that
// carries the op's marker is adopted (no POST); two or more are
// ambiguous_issue (no POST); none is exactly one create under the parent with
// the summary and the content. This last POST's failure is the protocol's to
// classify: a timeout or 5xx after it left is uncertain (remote_unknown,
// reconciled by the summary search), never not_sent (hk 4797).
func (a articleCreate) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
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
	parent, err := a.sendParent(ctx, ar, op, p)
	if err != nil {
		return "", err
	}
	hits, err := ar.FindArticles(ctx, ArticleSearch{ProjectShort: p.YTProjectShort, ProjectID: op.YTTargetID, Query: p.Summary, Summary: p.Summary, ParentID: parent})
	if err != nil {
		return "", &NotSentError{Err: fmt.Errorf("articles of %q: %w", p.Summary, err)}
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
	return ar.CreateArticle(ctx, ArticleCreate{ProjectID: op.YTTargetID, ParentID: parent, Summary: p.Summary, Content: text})
}

// sendParent is the parent Send puts the document's article under, classified
// for the protocol: a failed read, a busy family lane and a failed family
// lookup are not_sent (nothing was written), while a root POST the remote may
// have taken is returned as it is, so writeFailed records it remote_unknown and
// the evidence search reconciles it (hk 4797).
func (a articleCreate) sendParent(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, error) {
	if p.ParentFamily == "" || p.ParentArticleID != "" {
		parent, _, err := articleParent(ctx, ar, op, p)
		if err != nil {
			return "", &NotSentError{Err: err}
		}
		return parent, nil
	}
	return a.familyRoot(ctx, ar, op, p)
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
		return &NotSentError{Err: fmt.Errorf("legacy article %s: %w", p.LegacyArticleID, err)}
	case a.ID != p.LegacyArticleID || p.LegacyProjectID == "" || a.ProjectID != p.LegacyProjectID || p.LegacyMarker == "" || !strings.Contains(a.Content, p.LegacyMarker):
		return &RefusedError{Reason: ReasonLegacySyncedUnverified, Detail: fmt.Sprintf("legacy article %s (project %s) is not an article of the legacy project %q carrying the document's legacy marker",
			p.LegacyArticleID, a.ProjectID, p.LegacyProjectID)}
	}
	return &AdoptedError{ID: a.ID}
}

// evidenceParent is the parent Evidence searches the document's article under:
// the recorded root when the family has one, else the same lookup Send makes,
// never writing. Kept apart from articleParent so reconcile measures the family
// the send resolved, not merely the oldest root the project happens to hold.
func (a articleCreate) evidenceParent(ctx context.Context, ar ArticleRemote, op Op, p ArticleCreatePayload) (string, bool, error) {
	if p.ParentFamily != "" && p.ParentArticleID == "" {
		if id, ok, err := a.recordedRoot(ctx, ar, op, p); err != nil || ok {
			return id, ok, err
		}
	}
	return articleParent(ctx, ar, op, p)
}

// Evidence lists the project's articles with the op's summary. One that
// carries the op's marker and whose content is exactly the rendered text is
// a match; one that carries the marker with other content is a mismatch, so
// the shared rule keeps the op remote_unknown.
func (a articleCreate) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	ar, ok := remote.(ArticleRemote)
	if !ok {
		return ev, errors.New("the remote cannot list articles")
	}
	p, err := articleCreateOf(op)
	if err != nil {
		return ev, err
	}
	parent, found, err := a.evidenceParent(ctx, ar, op, p)
	if err != nil {
		return readFailure(ev, err)
	}
	if !found {
		return ev, nil
	}
	hits, err := ar.FindArticles(ctx, ArticleSearch{ProjectShort: p.YTProjectShort, ProjectID: op.YTTargetID, Query: p.Summary, Summary: p.Summary, ParentID: parent})
	if err != nil {
		return readFailure(ev, fmt.Errorf("articles of %q: %w", p.Summary, err))
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
