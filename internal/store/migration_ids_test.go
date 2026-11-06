package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func noop(context.Context, pgx.Tx) error { return nil }

func mig(id string, after ...string) migration {
	return migration{ID: id, After: after, Apply: noop}
}

func ids(ms []migration) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return strings.Join(out, ",")
}

// A registry that breaks a rule fails here with plain `go test`, no database.
func TestMigrationRegistryRejects(t *testing.T) {
	cases := []struct {
		name string
		reg  []migration
		want []string // every one must appear in the error
	}{
		{"duplicate", []migration{mig("20261106-a"), mig("20261106-b"), mig("20261106-a")}, []string{"20261106-a", "twice"}},
		{"too long", []migration{mig("20261106-" + strings.Repeat("x", 56))}, []string{"20261106-xxxx", "malformed"}},
		{"upper case", []migration{mig("20261106-Add-Col")}, []string{"20261106-Add-Col", "malformed"}},
		{"no date", []migration{mig("add-col")}, []string{"add-col", "malformed"}},
		{"no slug", []migration{mig("20261106")}, []string{"20261106", "malformed"}},
		{"underscore", []migration{mig("20261106-add_col")}, []string{"20261106-add_col", "malformed"}},
		{"not a date", []migration{mig("20261340-add-col")}, []string{"20261340-add-col", "not a date"}},
		{"unknown after", []migration{mig("20261106-a"), mig("20261107-b", "20261001-missing")}, []string{"20261107-b", "20261001-missing", "unknown"}},
		{"cycle", []migration{mig("20261106-a", "20261106-b"), mig("20261106-b", "20261106-a"), mig("20261106-c")}, []string{"20261106-a", "20261106-b", "cycle"}},
		{"self cycle", []migration{mig("20261106-a", "20261106-a")}, []string{"20261106-a", "cycle"}},
		{"no apply", []migration{{ID: "20261106-a"}}, []string{"20261106-a", "no Apply"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := orderMigrations(c.reg)
			if err == nil {
				t.Fatal("registry accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestMigrationCycleErrorOmitsInnocentIDs(t *testing.T) {
	_, err := orderMigrations([]migration{mig("20261106-a", "20261106-b"), mig("20261106-b", "20261106-a"), mig("20261106-free")})
	if err == nil || strings.Contains(err.Error(), "free") {
		t.Fatalf("err=%v", err)
	}
}

func TestMigrationRegistryOrder(t *testing.T) {
	a, b, c, d := "20261105-a", "20261106-b", "20261107-c", "20261108-d"
	cases := []struct {
		name string
		reg  []migration
		want string
	}{
		{"empty", nil, ""},
		{"sorted by id", []migration{mig(c), mig(a), mig(d), mig(b)}, strings.Join([]string{a, b, c, d}, ",")},
		{"after beats id", []migration{mig(a, c), mig(b), mig(c)}, strings.Join([]string{b, c, a}, ",")},
		{"after chain", []migration{mig(a, b), mig(b, c), mig(c), mig(d)}, strings.Join([]string{c, b, a, d}, ",")},
	}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			got, err := orderMigrations(x.reg)
			if err != nil || ids(got) != x.want {
				t.Fatalf("order %q err=%v, want %q", ids(got), err, x.want)
			}
			// The written order never changes the run order.
			rev := make([]migration, len(x.reg))
			for i := range x.reg {
				rev[len(x.reg)-1-i] = x.reg[i]
			}
			if got2, err := orderMigrations(rev); err != nil || ids(got2) != x.want {
				t.Fatalf("reversed order %q err=%v, want %q", ids(got2), err, x.want)
			}
		})
	}
}

// The shipped registry must be valid: a bad entry fails the build, not Open.
func TestShippedMigrationRegistryIsValid(t *testing.T) {
	if _, err := orderMigrations(migrationRegistry); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesInvalidRegistryNamingIDs(t *testing.T) {
	s := &Store{registry: []migration{mig("20261106-a", "20261106-nope")}}
	err := s.migrateIfBehind(t.Context())
	if err == nil || !strings.Contains(err.Error(), "20261106-nope") {
		t.Fatalf("err=%v", err)
	}
}

// --- database tests ---

// tableApply makes a migration that creates one table.
func tableApply(table string) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{`CREATE TABLE ` + table + ` (id INT PRIMARY KEY)`})
	}
}

func tableMig(id, table string, after ...string) migration {
	return migration{ID: id, After: after, Apply: tableApply(table)}
}

func openRegistry(t *testing.T, url string, reg []migration) *Store {
	t.Helper()
	s, err := open(t.Context(), url, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func appliedIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var got []string
	if err := pool.QueryRow(t.Context(), `SELECT coalesce(array_agg(id ORDER BY id), '{}') FROM schema_migrations`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func integerVersions(t *testing.T, pool *pgxpool.Pool) []int32 {
	t.Helper()
	var got []int32
	if err := pool.QueryRow(t.Context(), `SELECT array_agg(version ORDER BY version) FROM schema_version`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestFreshDatabaseMigratesIntegersThenIDs(t *testing.T) {
	_, url := testdb.Schema(t, "ids_fresh")
	reg := []migration{tableMig("20261107-second", "ids_second", "20261106-first"), tableMig("20261106-first", "ids_first")}
	s := openRegistry(t, url, reg)
	got, want := integerVersions(t, s.pool), migrateVersions()
	if len(got) != len(want) || int(got[len(got)-1]) != SchemaVersion {
		t.Fatalf("integer versions %v, want %v", got, want)
	}
	for i := range want {
		if int(got[i]) != want[i] {
			t.Fatalf("integer versions %v, want %v", got, want)
		}
	}
	if ids := appliedIDs(t, s.pool); strings.Join(ids, ",") != "20261106-first,20261107-second" {
		t.Fatalf("applied ids %v", ids)
	}
	if !tableExists(t, s.pool, "ids_first") || !tableExists(t, s.pool, "ids_second") {
		t.Fatal("registered migration did not run")
	}
	var stamped int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations WHERE abs(extract(epoch FROM now() - applied_at)) < 300`).Scan(&stamped); err != nil || stamped != 2 {
		t.Fatalf("applied_at set on %d rows err=%v", stamped, err)
	}
}

// A database that stopped at integer 52 (production before this change)
// gains integer 53 and then the IDs, keeping every row it had.
func TestIntegerFiftyTwoDatabaseGainsTableAndIDs(t *testing.T) {
	_, url := testdb.Schema(t, "ids_at52")
	old := openRegistry(t, url, nil)
	ctx := t.Context()
	if _, err := old.pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `DELETE FROM schema_version WHERE version=53`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `INSERT INTO memory(agent,name,description,memory_type,content,updated_by,updated_at) VALUES('a','kept','d','user','c','t',now())`); err != nil {
		t.Fatal(err)
	}
	if cur, err := migrateCurrent(ctx, old.pool, nil); err != nil || cur {
		t.Fatalf("schema at 52 current=%v err=%v", cur, err)
	}
	s := openRegistry(t, url, []migration{tableMig("20261106-first", "ids_first")})
	if got := integerVersions(t, s.pool); got[len(got)-1] != 53 {
		t.Fatalf("integer versions %v", got)
	}
	if ids := appliedIDs(t, s.pool); strings.Join(ids, ",") != "20261106-first" || !tableExists(t, s.pool, "ids_first") {
		t.Fatalf("applied ids %v", ids)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM memory WHERE name='kept'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row survived: %d err=%v", n, err)
	}
}

// holdMigrationLock takes migrationLockID in another transaction. A migrate
// that starts meanwhile blocks on its first statement; Open on a current
// database never starts one.
func holdMigrationLock(t *testing.T, url string) {
	t.Helper()
	pool := openPool(t, url)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err = tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		t.Fatal(err)
	}
}

func openPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	p, err := newPool(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestCurrentDatabaseOpensWithoutMigrating(t *testing.T) {
	_, url := testdb.Schema(t, "ids_current")
	reg := []migration{tableMig("20261106-first", "ids_first")}
	openRegistry(t, url, reg).Close()
	holdMigrationLock(t, url)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := open(ctx, url, reg)
	if err != nil {
		t.Fatalf("second Open on a current database entered migrate: %v", err)
	}
	s.Close()

	// Control: with one more unrecorded ID the same Open must reach migrate
	// and wait on the lock, so the pass above is not an accident of the setup.
	short, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	behind := append([]migration{tableMig("20261107-new", "ids_new")}, reg...)
	if s, err = open(short, url, behind); err == nil {
		s.Close()
		t.Fatal("Open with an unrecorded ID did not wait for the migration lock")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("control err=%v", err)
	}
}

// Registering two IDs in either order, or applying them in two separate
// releases in either order, ends in the same database.
func TestRegistrationOrderDoesNotChangeFinalState(t *testing.T) {
	a, b := tableMig("20261106-a", "ids_a"), tableMig("20261107-b", "ids_b")
	state := func(url string) string {
		s := openRegistry(t, url, []migration{})
		var tables []string
		if err := s.pool.QueryRow(t.Context(), `SELECT coalesce(array_agg(tablename ORDER BY tablename) FILTER (WHERE tablename LIKE 'ids\_%'), '{}') FROM pg_tables WHERE schemaname=current_schema()`).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		return strings.Join(tables, ",") + "|" + strings.Join(appliedIDs(t, s.pool), ",")
	}
	fresh := func(prefix string, regs ...[]migration) string {
		_, url := testdb.Schema(t, prefix)
		for _, reg := range regs {
			openRegistry(t, url, reg).Close()
		}
		return state(url)
	}
	ab := fresh("ids_ab", []migration{a, b})
	ba := fresh("ids_ba", []migration{b, a})
	aThenB := fresh("ids_a_then_b", []migration{a}, []migration{a, b})
	bThenA := fresh("ids_b_then_a", []migration{b}, []migration{a, b})
	want := "ids_a,ids_b|20261106-a,20261107-b"
	for name, got := range map[string]string{"ab": ab, "ba": ba, "a then b": aThenB, "b then a": bThenA} {
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// A newer binary may have recorded IDs and integer versions this one has
// never heard of; a rollback must still Open without migrating.
func TestUnknownRecordedIDsAndVersionsAreIgnored(t *testing.T) {
	_, url := testdb.Schema(t, "ids_unknown")
	reg := []migration{tableMig("20261106-first", "ids_first")}
	s := openRegistry(t, url, reg)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO schema_migrations(id, applied_at) VALUES ('29990101-from-the-future', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO schema_version(version) VALUES (9999)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	holdMigrationLock(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s2, err := open(ctx, url, reg)
	if err != nil {
		t.Fatalf("Open with unknown ids: %v", err)
	}
	defer s2.Close()
	if got := appliedIDs(t, s2.pool); len(got) != 2 {
		t.Fatalf("applied ids %v", got)
	}
}

// previousBinaryMigrateCurrent is migrateCurrent exactly as v0.0.28 and every
// binary up to handoffkeep@5ef9dc5 (internal/store/migration.go:55-81) wrote
// it: the integer list 1..52 without 8 and 20, schema_version only. It never
// reads schema_migrations.
func previousBinaryMigrateCurrent(ctx context.Context, q rowQuerier) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT current_schema() IS NOT NULL AND to_regclass(format('%I.schema_version', current_schema())) IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	var want []int
	for v := 1; v <= 52; v++ {
		if v != 8 && v != 20 {
			want = append(want, v)
		}
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version = ANY($1)`, want).Scan(&n); err != nil {
		return false, err
	}
	return n == len(want), nil
}

func TestPreviousBinaryMigrateCurrentStaysCurrent(t *testing.T) {
	_, url := testdb.Schema(t, "ids_prevbin")
	s := openRegistry(t, url, []migration{tableMig("20261106-first", "ids_first")})
	var rows int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("schema_migrations rows=%d err=%v", rows, err)
	}
	cur, err := previousBinaryMigrateCurrent(t.Context(), s.pool)
	if err != nil || !cur {
		t.Fatalf("previous binary sees the database current=%v err=%v", cur, err)
	}
	// Control: the pinned function really fails on a schema missing a version.
	if _, err = s.pool.Exec(t.Context(), `DELETE FROM schema_version WHERE version=52`); err != nil {
		t.Fatal(err)
	}
	if cur, err = previousBinaryMigrateCurrent(t.Context(), s.pool); err != nil || cur {
		t.Fatalf("control current=%v err=%v", cur, err)
	}
}
