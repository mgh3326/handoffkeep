package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func init() {
	registerMigration(migration{
		ID:    "20261109-yt-doc-legacy-writes",
		After: []string{"20261109-yt-doc-parts"},
		Apply: applyYouTrackDocLegacyWritesMigration,
	})
}

// Old releases update source_sha without knowing part_sha. The trigger invalidates that hash so the new sync repairs the article. CREATE TRIGGER takes SHARE ROW EXCLUSIVE on the ledger table only.
func applyYouTrackDocLegacyWritesMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE OR REPLACE FUNCTION clear_stale_youtrack_doc_part_sha() RETURNS trigger LANGUAGE plpgsql AS $body$
		BEGIN
			IF (NEW.hk_project IS DISTINCT FROM OLD.hk_project
			    OR NEW.article_id IS DISTINCT FROM OLD.article_id
			    OR NEW.source_sha IS DISTINCT FROM OLD.source_sha)
			   AND NEW.part_sha IS NOT DISTINCT FROM OLD.part_sha THEN
				NEW.part_sha := '';
			END IF;
			RETURN NEW;
		END;
		$body$`,
		`CREATE TRIGGER clear_stale_youtrack_doc_part_sha BEFORE UPDATE ON youtrack_doc_articles FOR EACH ROW EXECUTE FUNCTION clear_stale_youtrack_doc_part_sha()`,
	})
}
