package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	// binding (YTBindingOpApplied is reserved for an applied document op).
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
const YTArticleSafePartBytes = 240 * 1024

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

// YTArticleDoc is a listed document and the tenant of its binding ("" when it
// has none).
type YTArticleDoc struct {
	YTArticleCandidate
	BoundTenant string
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
	rows, err := s.pool.Query(ctx, ytArticleCandidateSQL+`, coalesce(b.tenant, '')`+ytArticleCandidateFrom+`
			LEFT JOIN yt_article_bindings b ON b.document_id = d.id
			WHERE d.id > $2 AND ($1 = '' OR `+ytArticleBucketSQL+` = $1) ORDER BY d.id LIMIT $3`, bucket, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTArticleDoc{}
	for rows.Next() {
		var d YTArticleDoc
		if err := scanYTArticleCandidate(rows, &d.YTArticleCandidate, &d.BoundTenant); err != nil {
			return nil, err
		}
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

var ytSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AddYTArticleBinding publishes a verified binding (origin import_verified).
// verified is the document as it was when its article was verified: the
// transaction reads it again and refuses with ErrYTArticleBindingConflict if
// it is gone or changed since (body, task or project), and refuses an
// excluded document outright. Adding the same binding again (same tenant
// and article) is a no-op that returns the stored row; any other binding for
// the document, or another document's binding to the article, is
// ErrYTArticleBindingConflict and changes nothing. No other row is written.
//
// Locks: none on existing rows (plain reads), then the new binding row; a
// concurrent insert of the same key waits on the unique index.
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
