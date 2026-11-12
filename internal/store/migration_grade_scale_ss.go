package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const gradeScaleSSMigrationID = "20261112-grade-scale-ss"

func init() {
	registerMigration(migration{ID: gradeScaleSSMigrationID, Apply: applyGradeScaleSSMigration})
}

// The enum checks must admit the new top grade before a writer stores it.
// The migration locks bench_grades before bench_catalog, matching both grade
// writers. Each ALTER takes ACCESS EXCLUSIVE briefly on its small table; the
// bounded lock wait prevents a queued DDL lock from stalling live reads.
func applyGradeScaleSSMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, []string{
		`ALTER TABLE bench_grades DROP CONSTRAINT bench_grades_grade_check, ADD CONSTRAINT bench_grades_grade_check CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')) NOT VALID`,
		`ALTER TABLE bench_catalog DROP CONSTRAINT bench_catalog_grade_check, ADD CONSTRAINT bench_catalog_grade_check CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')) NOT VALID`,
	}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}
