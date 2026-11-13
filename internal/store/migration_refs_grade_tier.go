package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-refs-grade-tier copies each task's grade and tier from the retired
// Linear connector's refs.linear into refs.grade and refs.tier (hk 4425), the
// keys this release writes and reads. Expand-only: refs.linear is left as it
// is, so the previous release still reads its grade during the blue-green
// overlap, and a key already set under refs.grade or refs.tier is never
// overwritten. The UPDATE takes ROW EXCLUSIVE on tasks and row locks on the
// tasks it changes only; each changed task writes one sync_log row through the
// sync_tasks trigger. updated_at is not touched: the values a reader sees
// (TaskRefs.GradeTier) do not change.
func init() {
	registerMigration(migration{ID: "20261113-refs-grade-tier", Apply: applyRefsGradeTierMigration})
}

const refsGradeTierBackfillSQL = `UPDATE tasks SET refs = refs || jsonb_strip_nulls(jsonb_build_object(
		'grade', CASE WHEN coalesce(refs->>'grade','')='' AND coalesce(refs->'linear'->>'grade','')<>'' THEN refs->'linear'->'grade' END,
		'tier', CASE WHEN coalesce(refs->>'tier','')='' AND coalesce(refs->'linear'->>'tier','')<>'' THEN refs->'linear'->'tier' END))
	WHERE jsonb_typeof(refs->'linear')='object'
	  AND ((coalesce(refs->>'grade','')='' AND coalesce(refs->'linear'->>'grade','')<>'')
	    OR (coalesce(refs->>'tier','')='' AND coalesce(refs->'linear'->>'tier','')<>''))`

func applyRefsGradeTierMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{refsGradeTierBackfillSQL})
}
