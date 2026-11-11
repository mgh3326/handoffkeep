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
// Each ALTER takes ACCESS EXCLUSIVE briefly on its small bench table; the
// constraints remain NOT VALID, preserving the previous check's behavior.
func applyGradeScaleSSMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE bench_catalog DROP CONSTRAINT bench_catalog_grade_check, ADD CONSTRAINT bench_catalog_grade_check CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')) NOT VALID`,
		`ALTER TABLE bench_grades DROP CONSTRAINT bench_grades_grade_check, ADD CONSTRAINT bench_grades_grade_check CHECK(grade IN ('SS','S+','S','A+','A','B','C','unrated')) NOT VALID`,
	})
}
