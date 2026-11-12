package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Article bindings (hk 4053, docs/youtrack-write-protocol.md §Article
// bindings). A document's yt_article_bindings row names the YouTrack article
// the import made for it, so the document kind (Y5b) updates that article
// instead of creating another. `youtrack articles-backfill` writes a row
// after it finds the article by the import's own rendering of the document
// (YTArticleSummary); a row is never overwritten.

// YTArticleBinding is a document's verified YouTrack article: the tenant, the
// stable internal article id (not the readable id) and the project it was
// verified in. Part is the article's part (0: the whole document, or the
// index article of a split one). ContentSHA is the sha256 of the content the
// article held when it was verified ("" for a split document's index, whose
// content the import generated). NeedsUpdate is set when that content is not
// the document's (sha differs, or the document is split): Y5b rewrites it.
type YTArticleBinding struct {
	DocumentID      int64     `json:"document_id"`
	Tenant          string    `json:"tenant"`
	ArticleID       string    `json:"article_id"`
	ArticleReadable string    `json:"article_readable"`
	ProjectID       string    `json:"project_id"`
	Part            int       `json:"part"`
	Generation      int       `json:"generation"`
	ContentSHA      string    `json:"content_sha,omitempty"`
	NeedsUpdate     bool      `json:"needs_update"`
	BoundBy         string    `json:"bound_by"`
	BoundAt         time.Time `json:"bound_at"`
	// Origin is set by the store: YTBindingImportVerified for a backfill
	// binding, YTBindingOpApplied for one an applied article_create
	// published (hk 4206; ContentSHA is then the sha256 of the body the op
	// wrote, without its provenance line and marker). A create that adopted
	// the legacy sync's article publishes YTBindingImportVerified.
	Origin string `json:"origin"`
	OpID   int64  `json:"op_id,omitempty"`
}

// ErrYTArticleBindingConflict is a binding that disagrees with one already
// stored (for the document, or for the article), or one verified against a
// document that changed since.
var ErrYTArticleBindingConflict = errors.New("yt_article_binding_conflict")

// YTArticleSafePartBytes is the import's SAFE_PART_BYTES
// (youtrack/import/client.js): a document body longer than this many UTF-8
// bytes was imported as an index article and its parts.
const YTArticleSafePartBytes = ytwrite.MaxArticleBytes

// ytArticleSummaryRunes is the import's summary cut (truncateRunes(..., 250)).
const ytArticleSummaryRunes = 250

// ytArticleIndexSuffix ends the summary of a split document's index article.
const ytArticleIndexSuffix = " (분할 문서)"

// YTArticleSummary is the summary the import gave the article of a whole
// document: "[<prefix>] <key> [<kind>]" cut to 250 code points, where prefix
// is the key's first path component (client.js documentArticles).
func YTArticleSummary(key, kind string) string {
	prefix, _, _ := strings.Cut(key, "/")
	r := []rune("[" + prefix + "] " + key + " [" + kind + "]")
	if len(r) > ytArticleSummaryRunes {
		r = r[:ytArticleSummaryRunes]
	}
	return string(r)
}

// ytArticleIndexSuffixRunes is the length of ytArticleIndexSuffix in code
// points.
var ytArticleIndexSuffixRunes = utf8.RuneCountInString(ytArticleIndexSuffix)

// ytArticleSafeRenderingRunes is the longest full rendering whose summaries
// can name only their own document: 250 - 8 - 1 = 241 code points.
var ytArticleSafeRenderingRunes = ytArticleSummaryRunes - ytArticleIndexSuffixRunes - 1

// YTArticleSummaryCut reports whether a document's article summaries cannot
// name it alone (decision-6, hk 4053); such a document is never bound
// (summary_cut). It is true when the full rendering
// "[<prefix>] <key> [<kind>]" is 242 code points or longer.
//
// The argument is by length. Let R be the full rendering. The import names a
// document's article YTArticleSummary (R cut to 250) or, for a split
// document, YTArticleIndexSummary (that, plus the 8 code points of
// " (분할 문서)", not cut again).
//
//   - When R is 241 code points or shorter, both strings this document can
//     match are shorter than 250: the summary is R itself (at most 241), and
//     the index is R plus the suffix (at most 249).
//   - Every cut rendering of any key is 250 code points or longer: a cut
//     summary is exactly 250, and a cut document's index is at least 258.
//     So no string this document matches is a cut rendering of another key.
//   - An uncut rendering names one key. The kind has no brackets, so the
//     final " [<kind>]" parses back unambiguously, and the prefix is the
//     key's own first component; the index adds a fixed suffix that ends in
//     ")", where a summary ends in "]".
//   - Part articles (" / part i of n") are children and are never matched.
//
// From 242 code points on, the index reaches 250 and can equal the cut
// summary of another, longer key; at exactly 250 the summary is not cut but
// has a cut summary's length, so it can equal one (finding r4545414568);
// from 251 on, the summary itself is cut. All are refused. So two keys can share a matchable summary only when the
// summary is cut, and this function returns true for every such document.
func YTArticleSummaryCut(key, kind string) bool {
	prefix, _, _ := strings.Cut(key, "/")
	return utf8.RuneCountInString("["+prefix+"] "+key+" ["+kind+"]") > ytArticleSafeRenderingRunes
}

// YTArticleIndexSummary is the summary of a split document's index article:
// YTArticleSummary and " (분할 문서)", not cut again.
func YTArticleIndexSummary(key, kind string) string {
	return YTArticleSummary(key, kind) + ytArticleIndexSuffix
}

// YTArticleSplit reports whether a body of bodyBytes UTF-8 bytes is imported
// as a split document (client.js splitUtf8 makes more than one piece).
func YTArticleSplit(bodyBytes int) bool { return bodyBytes > YTArticleSafePartBytes }

// YTArticleExcluded reports whether the import made no article for the
// document (client.js excludedDoc): a key under screens/, a review/mwr-*
// prompt, or a task's report (reports/mwr-*/..., reports/<job>/report.md),
// which became an issue comment instead. taskID 0 is no task.
func YTArticleExcluded(key string, taskID int64) bool {
	path := strings.Split(key, "/")
	switch {
	case path[0] == "screens":
		return true
	case path[0] == "review" && len(path) > 1 && strings.HasPrefix(path[1], "mwr-"):
		return true
	case taskID == 0 || path[0] != "reports" || len(path) < 2:
		return false
	case strings.HasPrefix(path[1], "mwr-"):
		return true
	default:
		return len(path) == 3 && path[2] == "report.md"
	}
}

// The import's synthetic article projects (client.js getProjects): documents
// with no task, and documents of a task with no project.
const (
	YTArticleBucketShared     = "/shared"
	YTArticleBucketUnassigned = "/unassigned"
)

// YTArticleCandidate is what an article binding is verified against: the
// document, and the project the import put its article in. Bucket is the hk
// project of the document's task, or YTArticleBucketShared /
// YTArticleBucketUnassigned; ShortName and YTProjectID are that project's
// youtrack_projects row ("" without one, and always for the two buckets).
type YTArticleCandidate struct {
	DocumentID  int64
	Key         string
	Kind        string
	SHA256      string
	BodyBytes   int
	TaskID      int64
	Bucket      string
	ShortName   string
	YTProjectID string
}

// YTArticleDoc is a listed document, when it was created, the tenant of its
// binding ("" when it has none) and whether that binding is a split
// document's index (no content_sha).
type YTArticleDoc struct {
	YTArticleCandidate
	CreatedAt       time.Time
	BoundTenant     string
	BoundSplitIndex bool
}

// ytArticleCandidateSQL reads candidates. The bucket is the import's
// document filter (ImportDocumentsPage): no task is /shared, a task with no
// project is /unassigned, otherwise the task's project.
const ytArticleBucketSQL = `CASE WHEN d.task_id IS NULL THEN '/shared' WHEN t.project IS NULL THEN '/unassigned' ELSE t.project END`

const ytArticleCandidateSQL = `SELECT d.id, d.key, d.kind, d.sha256, octet_length(d.body), coalesce(d.task_id, 0), ` + ytArticleBucketSQL + `,
		coalesce(p.short_name, ''), coalesce(p.yt_project_id, '')`

const ytArticleCandidateFrom = ` FROM documents d LEFT JOIN tasks t ON t.id = d.task_id LEFT JOIN youtrack_projects p ON p.hk_project = t.project`

func scanYTArticleCandidate(row interface{ Scan(...any) error }, c *YTArticleCandidate, extra ...any) error {
	return row.Scan(append([]any{&c.DocumentID, &c.Key, &c.Kind, &c.SHA256, &c.BodyBytes, &c.TaskID, &c.Bucket, &c.ShortName, &c.YTProjectID}, extra...)...)
}

// YTArticleDocPageMax bounds one page of YTArticleDocs.
const YTArticleDocPageMax = 1000

// YTArticleDocs lists, in id order, a page of documents with id greater than
// afterID, at most limit, with the tenant of each one's binding; bucket, when
// not "", keeps one bucket's (an hk project, /shared or /unassigned).
// Excluded documents are listed too: the caller counts them. Plain reads.
func (s *Store) YTArticleDocs(ctx context.Context, bucket string, afterID int64, limit int) ([]YTArticleDoc, error) {
	if afterID < 0 || limit < 1 || limit > YTArticleDocPageMax {
		return nil, errors.New("invalid article document filter")
	}
	rows, err := s.pool.Query(ctx, ytArticleCandidateSQL+`, d.created_at, coalesce(b.tenant, ''), b.document_id IS NOT NULL AND b.content_sha IS NULL`+ytArticleCandidateFrom+`
			LEFT JOIN yt_article_bindings b ON b.document_id = d.id
			WHERE d.id > $2 AND ($1 = '' OR `+ytArticleBucketSQL+` = $1) ORDER BY d.id LIMIT $3`, bucket, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTArticleDoc{}
	for rows.Next() {
		var d YTArticleDoc
		if err := scanYTArticleCandidate(rows, &d.YTArticleCandidate, &d.CreatedAt, &d.BoundTenant, &d.BoundSplitIndex); err != nil {
			return nil, err
		}
		d.CreatedAt = d.CreatedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

const ytArticleBindingColumns = `document_id, tenant, article_id, article_readable, project_id, part, generation, coalesce(content_sha, ''), origin, coalesce(op_id, 0), needs_update, bound_by, bound_at`

func scanYTArticleBinding(row interface{ Scan(...any) error }, b *YTArticleBinding) error {
	err := row.Scan(&b.DocumentID, &b.Tenant, &b.ArticleID, &b.ArticleReadable, &b.ProjectID, &b.Part, &b.Generation, &b.ContentSHA, &b.Origin, &b.OpID, &b.NeedsUpdate, &b.BoundBy, &b.BoundAt)
	b.BoundAt = b.BoundAt.UTC()
	return err
}

func readYTArticleBinding(ctx context.Context, q ytRowQuerier, documentID int64) (YTArticleBinding, bool, error) {
	var b YTArticleBinding
	err := scanYTArticleBinding(q.QueryRow(ctx, `SELECT `+ytArticleBindingColumns+` FROM yt_article_bindings WHERE document_id=$1`, documentID), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTArticleBinding{}, false, nil
	}
	if err != nil {
		return YTArticleBinding{}, false, err
	}
	return b, true, nil
}

// YTArticleBinding reads a document's binding; found is false when it has
// none.
func (s *Store) YTArticleBinding(ctx context.Context, documentID int64) (YTArticleBinding, bool, error) {
	return readYTArticleBinding(ctx, s.pool, documentID)
}

// lockYTArticleSourcesTx takes FOR SHARE, to commit, on what a candidate is
// read from: the document row, its task's row and the youtrack_projects row
// of the task's project (each when there is one). A missing document is
// ErrYTArticleBindingConflict. The order is PutDocument's (the document row,
// then its task through the foreign key); FOR SHARE on the task row blocks a
// project change, and no task or mapping writer locks a documents row while
// it holds either (lockorder.go).
func lockYTArticleSourcesTx(ctx context.Context, tx pgx.Tx, documentID int64) error {
	var taskID *int64
	err := tx.QueryRow(ctx, `SELECT task_id FROM documents WHERE id=$1 FOR SHARE`, documentID).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: document %d no longer exists", ErrYTArticleBindingConflict, documentID)
	}
	if err != nil || taskID == nil {
		return err
	}
	var project *string
	err = tx.QueryRow(ctx, `SELECT project FROM tasks WHERE id=$1 FOR SHARE`, *taskID).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && project == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT 1 FROM youtrack_projects WHERE hk_project=$1 FOR SHARE`, *project)
	return err
}

var ytSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AddYTArticleBinding publishes a verified binding (origin import_verified).
// verified is the document as it was when its article was verified: the
// transaction reads it again and refuses with ErrYTArticleBindingConflict if
// it is gone or changed since (body, task or project), and refuses an
// excluded document outright. Adding the same binding again (same tenant
// and article) is a no-op that returns the stored row; any other binding for
// the document, or another document's binding to the article, is
// ErrYTArticleBindingConflict and changes nothing. No other row is written.
// A document whose article_create op (hk 4206) is unresolved is refused with
// ErrYTArticleBindingConflict too: the op gives it its article, and a binding
// published under it would hold the op, and every op behind it on the
// document's lane, for good.
//
// In HANDOFFKEEP_YT_DOCS_MODE=on the new binding's article_update, when its
// article does not hold the document's body, is prepared in the same
// transaction (ytArticleSnapshotTx), after the insert, unless documents are
// canonical in YouTrack (then none is).
//
// Locks, in this order (lockorder.go): the document's lane lock (as the
// feed and an article_create's Apply take it, before any row), FOR SHARE on
// the documents yt_authority row (as prepareYTOpTx takes it; a switch holds
// it FOR UPDATE and takes no lane lock or documents row), then FOR SHARE on
// the documents row, then
// on its task's row, then on that project's youtrack_projects row, held to
// commit, so neither the body, the task, the task's project nor its mapping
// can change between the comparison and the insert; then the new binding
// row (a concurrent insert of the same key waits on the unique index).
func (s *Store) AddYTArticleBinding(ctx context.Context, b YTArticleBinding, verified YTArticleCandidate) (YTArticleBinding, error) {
	if b.DocumentID < 1 || verified.DocumentID != b.DocumentID || b.Tenant == "" || len(b.Tenant) > 255 || b.ArticleID == "" || len(b.ArticleID) > 64 ||
		len(b.ArticleReadable) > 64 || b.ProjectID == "" || len(b.ProjectID) > 64 || b.Part < 0 || b.Generation != 0 ||
		(b.ContentSHA != "" && !ytSHA256Hex.MatchString(b.ContentSHA)) || b.BoundBy == "" || !validText(b.BoundBy, 200) {
		return YTArticleBinding{}, errors.New("invalid article binding")
	}
	if YTArticleExcluded(verified.Key, verified.TaskID) {
		return YTArticleBinding{}, fmt.Errorf("invalid article binding: document %d (%s) has no imported article", b.DocumentID, verified.Key)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTArticleBinding{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, ytwrite.ArticleEntity, strconv.FormatInt(b.DocumentID, 10)); err != nil {
		return YTArticleBinding{}, err
	}
	// FOR SHARE on the documents authority row before the binding exists
	// (r4547807096): a switch (FOR UPDATE) either commits first, and this
	// transaction then sees documents canonical in YouTrack and queues no
	// update, or waits for the binding and its update and counts them.
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityDocuments)
	if err != nil {
		return YTArticleBinding{}, err
	}
	if err := lockYTArticleSourcesTx(ctx, tx, b.DocumentID); err != nil {
		return YTArticleBinding{}, err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops WHERE kind=$1 AND entity_kind=$2 AND entity_id=$3
		AND state NOT IN ('applied','rejected'))`, ytwrite.KindArticleCreate, ytwrite.ArticleEntity, strconv.FormatInt(b.DocumentID, 10)).Scan(&pending); err != nil {
		return YTArticleBinding{}, err
	}
	if pending {
		return YTArticleBinding{}, fmt.Errorf("%w: document %d has an unresolved article_create op", ErrYTArticleBindingConflict, b.DocumentID)
	}
	var current YTArticleCandidate
	err = scanYTArticleCandidate(tx.QueryRow(ctx, ytArticleCandidateSQL+ytArticleCandidateFrom+` WHERE d.id = $1`, b.DocumentID), &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTArticleBinding{}, fmt.Errorf("%w: document %d no longer exists", ErrYTArticleBindingConflict, b.DocumentID)
	}
	if err != nil {
		return YTArticleBinding{}, err
	}
	if current != verified {
		return YTArticleBinding{}, fmt.Errorf("%w: document %d changed after verification (verified %s sha %s in %s, now %s sha %s in %s); verify again",
			ErrYTArticleBindingConflict, b.DocumentID, verified.Key, verified.SHA256, verified.Bucket, current.Key, current.SHA256, current.Bucket)
	}
	var contentSHA any
	if b.ContentSHA != "" {
		contentSHA = b.ContentSHA
	}
	var got YTArticleBinding
	err = scanYTArticleBinding(tx.QueryRow(ctx, `INSERT INTO yt_article_bindings(document_id, tenant, article_id, article_readable, project_id, part, generation, content_sha, origin, needs_update, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,$5,$6,0,$7,$8,$9,$10,$11)
		ON CONFLICT DO NOTHING RETURNING `+ytArticleBindingColumns,
		b.DocumentID, b.Tenant, b.ArticleID, b.ArticleReadable, b.ProjectID, b.Part, contentSHA, YTBindingImportVerified, b.NeedsUpdate, b.BoundBy, time.Now().UTC()), &got)
	if err == nil {
		// In docs mode on (hk 4206), a binding whose article does not hold
		// the document's body gets its article_update in the same
		// transaction: no feed row comes for a binding, and the documents
		// authority switch must not see it published with no op behind it.
		// Once documents are canonical in YouTrack, a PG body is never
		// written over the article: the binding is kept, its update is not.
		if side == YTAuthorityHK {
			if _, err := s.ytArticleSnapshotTx(ctx, tx, b.DocumentID, time.Now().UTC()); err != nil {
				return YTArticleBinding{}, err
			}
		}
		return got, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return YTArticleBinding{}, err
	}
	have, found, err := readYTArticleBinding(ctx, tx, b.DocumentID)
	switch {
	case err != nil:
		return YTArticleBinding{}, err
	case !found:
		return YTArticleBinding{}, fmt.Errorf("%w: article %s on %s is bound to another document", ErrYTArticleBindingConflict, b.ArticleID, b.Tenant)
	case have.Tenant != b.Tenant || have.ArticleID != b.ArticleID:
		return YTArticleBinding{}, fmt.Errorf("%w: document %d is bound to article %s on %s", ErrYTArticleBindingConflict, b.DocumentID, have.ArticleID, have.Tenant)
	}
	return have, tx.Commit(ctx)
}
