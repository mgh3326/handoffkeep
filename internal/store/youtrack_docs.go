package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// YouTrackDocArticle is the ledger row for one mirrored hk document, or one
// per-prefix parent article (doc_key "index:<prefix>:<hk project>").
type YouTrackDocArticle struct {
	DocKey    string    `json:"doc_key"`
	HKProject string    `json:"hk_project"`
	ArticleID string    `json:"article_id"`
	SourceSHA string    `json:"source_sha"`
	Bytes     int64     `json:"bytes"`
	SyncedAt  time.Time `json:"synced_at"`
}

// DueYouTrackDocs returns up to limit documents whose key matches one of the
// LIKE patterns (for example "design/%") and that are due: no article yet, a
// sha256 that differs from the one last written, or an article in a different
// YouTrack project than the document's project now resolves to. A document
// resolves to its task's project, or defaultProject when it has no task or a
// blank project, and falls back to the "other" mapping when that name has none,
// as YouTrackProjectFor does. A key parked by its attempt state is not due
// until its next_attempt_at. Ordered by that time and then by key, so a
// document that failed or was skipped waits behind the rest.
func (s *Store) DueYouTrackDocs(ctx context.Context, patterns []string, defaultProject string, limit int) ([]Document, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.key,d.kind,d.session,d.job,d.body,d.sha256,d.created_by,d.created_at,d.updated_at,d.task_id,COALESCE(d.origin_class,'')
		FROM documents d
		LEFT JOIN youtrack_doc_articles a ON a.doc_key=d.key
		LEFT JOIN tasks t ON t.id=d.task_id
		LEFT JOIN LATERAL (
		  SELECT p.hk_project FROM youtrack_projects p
		  WHERE p.hk_project IN (COALESCE(NULLIF(BTRIM(t.project),''),$3),'other')
		  ORDER BY (p.hk_project=COALESCE(NULLIF(BTRIM(t.project),''),$3)) DESC
		  LIMIT 1) rp ON true
		LEFT JOIN youtrack_attempts r ON r.queue='doc' AND r.item_key=d.key
		WHERE d.key LIKE ANY($1::text[])
		  AND (a.doc_key IS NULL OR a.source_sha<>d.sha256
		       OR a.hk_project<>COALESCE(rp.hk_project,''))
		  AND `+youtrackAttemptRank("r")+` <= now()
		ORDER BY `+youtrackAttemptRank("r")+` ASC, d.key ASC LIMIT $2`, patterns, limit, defaultProject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var x Document
		if err := rows.Scan(&x.ID, &x.Key, &x.Kind, &x.Session, &x.Job, &x.Body, &x.SHA256, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.TaskID, &x.OriginClass); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// OrphanYouTrackDocArticles lists ledger rows whose hk document no longer
// exists. Deletes are not propagated; the sync only reports them.
func (s *Store) OrphanYouTrackDocArticles(ctx context.Context, limit int) ([]YouTrackDocArticle, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT a.doc_key,a.hk_project,a.article_id,a.source_sha,a.bytes,a.synced_at
		FROM youtrack_doc_articles a
		WHERE a.doc_key NOT LIKE 'index:%' AND NOT EXISTS (SELECT 1 FROM documents d WHERE d.key=a.doc_key)
		ORDER BY a.doc_key ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YouTrackDocArticle{}
	for rows.Next() {
		var x YouTrackDocArticle
		if err := rows.Scan(&x.DocKey, &x.HKProject, &x.ArticleID, &x.SourceSHA, &x.Bytes, &x.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetYouTrackDocArticle reads one ledger row.
func (s *Store) GetYouTrackDocArticle(ctx context.Context, docKey string) (YouTrackDocArticle, bool, error) {
	var x YouTrackDocArticle
	err := s.pool.QueryRow(ctx, `SELECT doc_key,hk_project,article_id,source_sha,bytes,synced_at FROM youtrack_doc_articles WHERE doc_key=$1`, docKey).
		Scan(&x.DocKey, &x.HKProject, &x.ArticleID, &x.SourceSHA, &x.Bytes, &x.SyncedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return YouTrackDocArticle{}, false, nil
	}
	if err != nil {
		return YouTrackDocArticle{}, false, err
	}
	return x, true, nil
}

// UpsertYouTrackDocArticle records the article written for a document. The
// caller supplies synced_at from its own clock.
func (s *Store) UpsertYouTrackDocArticle(ctx context.Context, x YouTrackDocArticle) error {
	if x.DocKey == "" || x.HKProject == "" || x.ArticleID == "" || x.Bytes < 0 {
		return errors.New("invalid youtrack doc article")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO youtrack_doc_articles(doc_key,hk_project,article_id,source_sha,bytes,synced_at)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(doc_key) DO UPDATE SET hk_project=EXCLUDED.hk_project, article_id=EXCLUDED.article_id,
			source_sha=EXCLUDED.source_sha, bytes=EXCLUDED.bytes, synced_at=EXCLUDED.synced_at`,
		x.DocKey, x.HKProject, x.ArticleID, x.SourceSHA, x.Bytes, x.SyncedAt)
	return err
}
