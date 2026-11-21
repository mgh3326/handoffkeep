package store

import (
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
