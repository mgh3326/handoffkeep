package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migrations after integer version 53 are identified by a unique string ID,
// not a number. Integer versions made every schema PR claim "the next free
// number", so each schema merge forced every other open schema PR to renumber
// and re-run CI and review (hk 3180). An ID is chosen by its author and
// conflicts with nobody: two PRs that each add a registry entry merge in
// either order without touching each other's code.
//
// Version 53 is the last integer version. It creates schema_migrations, where
// the runner records each applied ID; the registry itself starts empty rather
// than with "create schema_migrations" as its first entry, because the table
// must exist before the runner can record anything, and a registry entry
// would have to create the table it is recorded in. migrateCurrent reads
// schema_migrations only after every integer version is recorded, so it never
// queries a table that integer 53 has not made yet.
//
// To add a migration, create migration_<slug>.go whose init() calls
// registerMigration; see AGENTS.md §Migrations. Each migration lives in its
// own file and no shared list is edited, so parallel PRs do not conflict. The
// runner (migrateRegistered) records the ID itself in the same transaction as
// Apply, so Apply must not insert into schema_migrations.

// schemaMigrationsV53 creates the table of applied migration IDs. It takes
// ACCESS EXCLUSIVE on nothing that exists yet (a new table), so it blocks no
// live writer; Store.migrate runs it under migrationLockID like every version.
//
// Binaries from before this version never read schema_migrations: their
// migrateCurrent counts schema_version rows only, so a database that has this
// table still opens with them (TestPreviousBinaryMigrateCurrentStaysCurrent).
var schemaMigrationsV53 = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		id TEXT PRIMARY KEY CHECK(id ~ '^[0-9]{8}-[a-z0-9-]+$' AND length(id) <= 64),
		applied_at TIMESTAMPTZ NOT NULL)`,
	`INSERT INTO schema_version(version) VALUES (53)`,
}

// migration is one registered, ID-identified schema step.
type migration struct {
	// ID is "YYYYMMDD-<slug>": the date the migration was written and a
	// lower-case [a-z0-9-] slug, at most maxMigrationIDLen bytes in all.
	ID string
	// After names IDs that must be applied before this one. Use it only for a
	// real dependency (a table or column this step needs). Steps with no
	// dependency run in ID order.
	After []string
	// Apply runs the statements inside the migrating transaction. It must not
	// record the ID: migrateRegistered does.
	Apply func(ctx context.Context, tx pgx.Tx) error
}

// migrationRegistry holds every ID-identified migration, filled by init()
// functions. Its order depends on file names and does not matter:
// orderMigrations sorts it.
var migrationRegistry []migration

// registerMigration adds m to the registry. Call it from init() only.
func registerMigration(m migration) { migrationRegistry = append(migrationRegistry, m) }

const maxMigrationIDLen = 64

var migrationIDRE = regexp.MustCompile(`^[0-9]{8}-[a-z0-9-]+$`)

// checkMigrationID explains why id is not a valid migration ID.
func checkMigrationID(id string) error {
	if len(id) > maxMigrationIDLen || !migrationIDRE.MatchString(id) {
		return fmt.Errorf("migration id %q is malformed: want YYYYMMDD-<slug> with [a-z0-9-], at most %d bytes", id, maxMigrationIDLen)
	}
	if _, err := time.Parse("20060102", id[:8]); err != nil {
		return fmt.Errorf("migration id %q is malformed: %q is not a date", id, id[:8])
	}
	return nil
}

// orderMigrations validates reg and returns it in run order: the ready entry
// with the smallest ID first, where an entry is ready once everything in its
// After list has been placed. The order depends only on the registry's
// content, never on the order its entries were written in.
func orderMigrations(reg []migration) ([]migration, error) {
	byID := make(map[string]migration, len(reg))
	for _, m := range reg {
		if err := checkMigrationID(m.ID); err != nil {
			return nil, err
		}
		if _, dup := byID[m.ID]; dup {
			return nil, fmt.Errorf("migration id %q is registered twice", m.ID)
		}
		if m.Apply == nil {
			return nil, fmt.Errorf("migration %q has no Apply", m.ID)
		}
		byID[m.ID] = m
	}
	for _, m := range reg {
		for _, dep := range m.After {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("migration %q is after unknown migration %q", m.ID, dep)
			}
		}
	}
	placed := make(map[string]bool, len(reg))
	out := make([]migration, 0, len(reg))
	for len(out) < len(reg) {
		next := ""
		for id, m := range byID {
			if placed[id] || (next != "" && id >= next) {
				continue
			}
			ready := true
			for _, dep := range m.After {
				ready = ready && placed[dep]
			}
			if ready {
				next = id
			}
		}
		if next == "" {
			return nil, fmt.Errorf("migration cycle among %s", strings.Join(cycleIDs(byID, placed), ", "))
		}
		placed[next] = true
		out = append(out, byID[next])
	}
	return out, nil
}

// cycleIDs names the unplaced IDs that sit on a cycle: it prunes the unplaced
// entries nothing unplaced depends on, which leaves the cycles and the entries
// on a path between them.
func cycleIDs(byID map[string]migration, placed map[string]bool) []string {
	left := map[string]bool{}
	for id := range byID {
		if !placed[id] {
			left[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		wanted := map[string]bool{}
		for id := range left {
			for _, dep := range byID[id].After {
				if left[dep] {
					wanted[dep] = true
				}
			}
		}
		for id := range left {
			if !wanted[id] {
				delete(left, id)
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(left))
	for id := range left {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// migrateRegistered applies every entry of ordered (from orderMigrations)
// that schema_migrations does not record yet, recording each ID in tx right
// after its Apply. It runs after the integer versions, inside Store.migrate's
// transaction and under migrationLockID.
func migrateRegistered(ctx context.Context, tx pgx.Tx, ordered []migration) error {
	for _, m := range ordered {
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=$1)`, m.ID).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := m.Apply(ctx, tx); err != nil {
			return fmt.Errorf("migration %s: %w", m.ID, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(id, applied_at) VALUES ($1, now())`, m.ID); err != nil {
			return fmt.Errorf("migration %s: record: %w", m.ID, err)
		}
	}
	return nil
}

// registeredCurrent reports whether schema_migrations records every ID in
// ordered. IDs the database has beyond ordered (a newer binary migrated it)
// do not count against it.
func registeredCurrent(ctx context.Context, q rowQuerier, ordered []migration) (bool, error) {
	if len(ordered) == 0 {
		return true, nil
	}
	ids := make([]string, len(ordered))
	for i, m := range ordered {
		ids[i] = m.ID
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return false, err
	}
	return n == len(ids), nil
}
