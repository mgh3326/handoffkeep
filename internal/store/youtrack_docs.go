package store

import (
	"context"
	"errors"
	"time"
)

// YouTrackDocArticle is the ledger row for one mirrored hk document, or one
// per-prefix parent article (doc_key "index:<prefix>:<hk project>").
type YouTrackDocArticle struct {
	DocKey      string    `json:"doc_key"`
	DocumentKey string    `json:"document_key"`
	PartIndex   int       `json:"part_index"`
	PartCount   int       `json:"part_count"`
	PartSHA     string    `json:"part_sha"`
	HKProject   string    `json:"hk_project"`
	ArticleID   string    `json:"article_id"`
	SourceSHA   string    `json:"source_sha"`
	Bytes       int64     `json:"bytes"`
	SyncedAt    time.Time `json:"synced_at"`
}

// UpsertYouTrackDocArticle records the article written for a document. The
// caller supplies synced_at from its own clock.
func (s *Store) UpsertYouTrackDocArticle(ctx context.Context, x YouTrackDocArticle) error {
	if x.DocKey == "" || x.HKProject == "" || x.ArticleID == "" || x.Bytes < 0 {
		return errors.New("invalid youtrack doc article")
	}
	if x.DocumentKey == "" {
		x.DocumentKey = x.DocKey
	}
	if x.PartIndex == 0 {
		x.PartIndex = 1
	}
	if x.PartCount == 0 {
		x.PartCount = 1
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO youtrack_doc_articles(doc_key,document_key,part_index,part_count,part_sha,hk_project,article_id,source_sha,bytes,synced_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(doc_key) DO UPDATE SET document_key=EXCLUDED.document_key,part_index=EXCLUDED.part_index,part_count=EXCLUDED.part_count,part_sha=EXCLUDED.part_sha,hk_project=EXCLUDED.hk_project, article_id=EXCLUDED.article_id,
			source_sha=EXCLUDED.source_sha, bytes=EXCLUDED.bytes, synced_at=EXCLUDED.synced_at`,
		x.DocKey, x.DocumentKey, x.PartIndex, x.PartCount, x.PartSHA, x.HKProject, x.ArticleID, x.SourceSHA, x.Bytes, x.SyncedAt)
	return err
}
