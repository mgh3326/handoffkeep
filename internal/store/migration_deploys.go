package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Migration 20261106-deploy-records adds the work record's deploy tables
// (hk 3036; design hk:doc design/2026-11-06/hk-work-record-design-v1.2 §2, §5 and §13.1 R6). Until
// now a deploy existed only as a deploy-record document, so hk could not say
// which changes an attempt shipped to which target, nor which merged changes
// still wait for a deploy. services is the operator's registry of deployable
// services; deploys holds one row per attempt (deploy or rollback);
// deploy_changes freezes, at start, which candidate changes the attempt
// carries for its service (included) and which the path filter left out.
//
// All three are new tables. The foreign keys take a short SHARE ROW EXCLUSIVE
// lock on changes (deploy_changes.change_id) and on services (deploys.service)
// while the constraints are added; the referencing tables are empty, so the
// validation is instant. No existing row is read or written.
var deploysMigration = []string{
	`CREATE TABLE IF NOT EXISTS services (
		name TEXT PRIMARY KEY,
		repo TEXT NOT NULL,
		path_patterns TEXT[] NOT NULL CHECK(cardinality(path_patterns) > 0),
		deploy_mode TEXT NOT NULL CHECK(deploy_mode IN ('manual','auto','none')),
		targets TEXT[] NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		CHECK(deploy_mode = 'none' OR cardinality(targets) > 0)
	)`,
	`CREATE TABLE IF NOT EXISTS deploys (
		id BIGSERIAL PRIMARY KEY,
		attempt_id TEXT NOT NULL UNIQUE,
		service TEXT NOT NULL REFERENCES services(name),
		targets TEXT[] NOT NULL CHECK(cardinality(targets) > 0),
		action TEXT NOT NULL CHECK(action IN ('deploy','rollback')),
		previous_ref TEXT,
		deployed_ref TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('requested','running','success','failed','rolled_back')),
		started_at TIMESTAMPTZ NOT NULL,
		finished_at TIMESTAMPTZ,
		failed_step TEXT,
		job_id TEXT,
		approval_ref TEXT,
		source TEXT NOT NULL CHECK(source IN ('installer','mergewake')),
		payload_hash TEXT NOT NULL,
		CHECK((finished_at IS NULL) = (state IN ('requested','running')))
	)`,
	`CREATE INDEX IF NOT EXISTS deploys_service ON deploys(service, id)`,
	`CREATE TABLE IF NOT EXISTS deploy_changes (
		deploy_id BIGINT NOT NULL REFERENCES deploys(id),
		change_id BIGINT NOT NULL REFERENCES changes(id),
		included BOOLEAN NOT NULL,
		matched_pattern TEXT,
		PRIMARY KEY(deploy_id, change_id),
		CHECK(included = (matched_pattern IS NOT NULL))
	)`,
	`CREATE INDEX IF NOT EXISTS deploy_changes_change ON deploy_changes(change_id)`,
}

func init() {
	registerMigration(migration{ID: "20261106-deploy-records", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, deploysMigration)
	}})
}
