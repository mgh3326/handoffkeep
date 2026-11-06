package store

// Version 50 creates a new table and takes no lock on an existing table.
// Its partial unique index enforces one active order per scope and topic.
var standingOrdersV50 = []string{
	`CREATE TABLE standing_orders (
 id BIGSERIAL PRIMARY KEY, scope_kind TEXT NOT NULL CHECK (scope_kind IN ('project','lane')),
 scope TEXT NOT NULL, topic_key TEXT NOT NULL CHECK (topic_key ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 statement TEXT NOT NULL, source_doc TEXT NOT NULL, decided_by TEXT NOT NULL,
 decided_at TIMESTAMPTZ NOT NULL, supersedes_id BIGINT UNIQUE REFERENCES standing_orders(id),
 superseded_at TIMESTAMPTZ, recorded_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
	`CREATE UNIQUE INDEX standing_orders_active ON standing_orders (scope_kind, scope, topic_key) WHERE superseded_at IS NULL`,
	`INSERT INTO schema_version(version) VALUES (50)`,
}
