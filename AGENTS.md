# Working in handoffkeep

Conventions for anyone changing this repo, people and coding agents alike.
Each concern has one pattern; when existing code differs, follow this file
and leave the old code as is unless the change already touches it.

## Layout

- `cmd/handoffkeep` — the single binary: CLI subcommands and `serve`.
- `internal/store` — Postgres access and every schema migration.
- `internal/api` — the bearer-token `/v1` JSON API.
- `internal/ui` — the Access-authenticated `/ui` console server; its React
  bundle is built from `web/console` into `internal/ui/static/console`
  (committed).
- `internal/remote` — the HTTP client the CLI uses against `/v1`.
- `client` — the public `/v1` client: the one transport and the fleet-facing
  wire types. It never imports `internal/store` (`client/README.md`).
- `internal/testdb` — how tests reach Postgres.
- `tests/` — cross-package integration tests against a real server and DB.

## Checks

`scripts/check` runs CI's gates in CI's order (`scripts/check go`,
`scripts/check web` for one half). Run it before pushing.

CI has three tiers (`.github/workflows/ci.yml` plus `deploy.yml`). A pull
request runs the light jobs only: no `-race`, hosted runners, PostgreSQL only
for the DB-backed packages it changes (below). The main tier is every other non-PR event: the light
jobs plus `main-linux`, which runs the whole Go suite against PostgreSQL
**without** `-race`, the migration/startup smoke, the reopen/quota mutants and
bundle freshness, targeting 10 minutes; its aggregate is `ci-ok`. The deploy
tier (`deploy.yml`, `suite=deploy`) runs `-race` with PostgreSQL, the same
suite on macOS, the release-build dry run, and the previous-release schema
check (the target migrates, the previous release must still serve) after a
successful main CI run, never cancelling, and publishes the `deploy-ok` commit
status; release.yml
still gates on `ci-ok` until mergewake reads `deploy_checks` (its `GATE` value
is the one-line switch). A red main head is bisected and reverted.
`ci-any-ref` accepts `packages` only with `suite=test`; those runs post `ci-ok-test-packages`.
`ci-any-ref suite=deploy` runs the deploy tier as a diagnostic and posts
`ci-ok-deploy`, never `deploy-ok`. `scripts/check go` runs the heavy half locally.
The PR tier runs PostgreSQL only in `pr: store + postgres`, and there for every
Go package the PR changes that has DB-backed tests: a `_test.go` file in it
imports `internal/testdb` at the PR head, and a changed or removed file belongs
to the nearest enclosing directory with Go files. A change under
`internal/store/` or `internal/testdb/` also runs `./internal/store/...`. If
the changed files cannot be listed it tests every DB-backed package, and if
that set cannot be computed it tests `./...`. Store breaks merged twice and
left main red (hk 3193), and so did an `internal/mcp` change whose DB tests
first ran on main (hk 3528). A package becomes DB-backed by importing
`internal/testdb`; no workflow edit is needed.

DB-backed tests need Postgres 17 and `HANDOFFKEEP_TEST_DB_URL`, e.g.

```bash
docker run -d --name hk-pg -p 5432:5432 -e POSTGRES_USER=handoffkeep -e POSTGRES_PASSWORD=handoffkeep -e POSTGRES_DB=handoffkeep postgres:17
export HANDOFFKEEP_TEST_DB_URL='postgres://handoffkeep:handoffkeep@localhost:5432/handoffkeep?sslmode=disable'
```

Toolchains are pinned: Go from `go.mod`, Node from `web/console/.nvmrc`.
Workflow actions are pinned to full commit SHAs, one version per action
across all workflows; `internal/ciworkflow` enforces both.

## Go

**API errors** (`internal/api/errors.go`). Error bodies are
`{"error": "<snake_code>"}` plus optional detail keys.
- A failure the handler detects itself: `writeError(w, http.StatusX, "code")`.
  Malformed ids and query values are `writeError(w, http.StatusBadRequest,
  "invalid_context")`; never `appErr(w, errors.New(...))`.
- An error returned by the store or a service: `appErr(w, r, err)`. The
  request lets a 500 carry its `request_id` and log the route.
- A new store sentinel the client must tell apart: declare
  `ErrX = errors.New("x_code")` in the store and add one row to `errorRules`.
  The wire code is the sentinel's text (a `x_not_found` sentinel may answer
  the shared `not_found`); `TestErrorRuleCodesMatchSentinels` enforces it.
  Set `reason: true` only when the text after `"x_code: "` is store-authored.
- Path ids are parsed with `pathID(r, "id")`; the handler checks the range.
  Cursor query parameters (`after_id`, `after`) go through `queryCursor`,
  limits through `queryLimit`.
- Unmapped database/network failures become `500 internal_error` and are
  logged; plain validation errors become `400 invalid_context`. Do not add a
  catch-all that hides a 500 as a 400.
- Use `http.StatusX` constants, not numeric literals.

**Console errors** (`internal/ui`). `/ui/api/*` JSON routes answer with
`writeJSONError(w, status, code)` and `writeBoardJSON`; HTML and form routes
keep `http.Error` with a short text message.

**Request bodies.** Decode JSON with `decode(r, &v, max)` (or
`decodeLimited`): size-capped, unknown fields and trailing data rejected.
`max` is one of the named caps (`bodyTiny`, `bodySmall`, `bodyEvent`,
`bodyLarge`) or a store-owned limit, never a bare number.

**Store.** Validation lives in the store; handlers do not re-validate.
Transactions use the existing idiom:

```go
tx, err := s.pool.Begin(ctx)
if err != nil {
	return err
}
defer tx.Rollback(ctx)
// ...
return tx.Commit(ctx)
```

Not-found reads return `(T, found bool, error)`, or a `ErrXNotFound`
sentinel when the caller is an API route. Check `pgx.ErrNoRows` with
`errors.Is`, never by error text. Wrap with `%w`; `fmt.Errorf("%w: ...: %v",
ErrX, err)` is the one exception, for when only the sentinel may be matched.
Every `ORDER BY` that feeds a `LIMIT` ends in a unique column (usually `id`)
so pages are deterministic. A jsonb column is scanned into `[]byte` and read
with `scanJSONB` (SQL NULL and JSON null leave the target unset). Shared
validators live in the store and are exported once (`ValidHostID`,
`validText`); other packages call them instead of copying the regex. A
validator that answers yes/no is `validX` and returns `bool`; one that
explains a refusal is `checkX` and returns `error`.

**Time.** Go-side timestamps are `time.Now().UTC()`, and an injectable clock
(`Now func() time.Time`) returns `.UTC()` too. One table or worker uses one
clock: either Go time passed in as a parameter, or the database's `now()`,
never both.

**Secret refusals.** `guard.Reject` returns a `*guard.SecretError`; branch on
it with `guard.SecretPattern(err)` (or `remote.SecretPattern` for a server
refusal), never on the `secret_like_content:` text.

**Migrations** (`internal/store/migration.go`, `migration_ids.go`). The integer
versions 1-53 are frozen: the comment in `migration.go` lists which function
owns each, 53 (`schema_migrations`) is the last integer ever, and
`SchemaVersion` stays 53. Never add or renumber an integer version; that made
every schema PR renumber when another one merged (hk 3180).

A new migration is a registry entry with a unique string ID
`YYYYMMDD-<slug>` (the date you write it; lower-case `[a-z0-9-]`, at most 64
bytes):
1. Create `internal/store/migration_<slug>.go` with the statements and an
   `init()` that calls `registerMigration(migration{ID: "YYYYMMDD-<slug>",
   Apply: ...})`. `Apply` runs the statements with `execAll(ctx, tx, ...)`.
   Edit no shared list, so parallel PRs merge in either order.
2. Do not insert into `schema_migrations`: `migrateRegistered` records the ID in
   the same transaction after `Apply` returns.
3. Set `After: []string{"<other id>"}` only for a real dependency (a table or
   column your statements need). Without it entries run in ID order, so
   do not use `After` to pick a position. An unknown ID or a cycle fails
   startup, and `TestShippedMigrationRegistryIsValid` fails the build.
4. Say in a comment why it is needed and what lock it takes.

Every schema change needs a new registered ID, even an additive `IF NOT EXISTS`
statement: `Open` runs no DDL and takes no table lock on a schema that
already records every integer version and every registered ID
(`migrateIfBehind`). A database holding IDs or versions this binary does not
know (a newer binary migrated it) still opens. Store locks follow the order in
`internal/store/lockorder.go`.

**Migrations are expand-only.** Blue-green migrates the shared database from
the new colour while the previous release keeps serving, so one release's
migration must leave the previous release readable and writable: a migration
may add tables, columns and indexes, and backfill values, but it must not drop
or rename a table or column, narrow a column's type or constraints, or add a
`NOT NULL` without a default on a column the previous release writes. Dropping,
renaming or narrowing anything the previous release still uses needs a
two-release contract: expand in this release, keep the old shape working, and
contract only after the release that stopped using it has itself been
superseded. The deploy tier enforces the observable part:
`scripts/test-previous-release-schema.sh` (the `schema-compat` job, hk 3511)
migrates a fresh database with the target binary, then runs the previous
release's server on that schema and exercises healthz, task list/show/create,
comment create and list, and the decision list; a failure fails `deploy-ok`.

**Remote client.** Branch on server error codes with
`remote.ErrorCode(err) == "not_found"`, never by comparing `err.Error()`.

**CLI.** Output is JSON via `printJSON` unless a command documents a text
form behind `--json`. Errors go to stderr; exit codes other than 1 are named
constants. A subcommand that takes positionals and flags in any order runs
its args through `splitArgs(args, "value-flag", ...)` (`args.go`) before
`fs.Parse`.

**Logging.** `log.Printf` with one `event key=value ...` line
(e.g. `api.internal_error err="..."`). The event name is a literal, not built
at run time; a failure event ends in `_failed`; ids use the column name
(`task_id=`, not `task=`); free-form values are `%q`.

## Tests (Go)

- Reach Postgres only through `internal/testdb`: `testdb.URL(t)`,
  `testdb.Conn(t)`, or `testdb.Schema(t, prefix)` / `testdb.SchemaPool(t,
  prefix)`. Never read `HANDOFFKEEP_TEST_DB_URL` directly. Without it a test
  skips locally and fails in CI, except in a CI job that sets
  `HANDOFFKEEP_TEST_NO_DB=1` (the PR tier and macOS).
- Tests that count whole tables, truncate, seed fixed keys or run migration
  DDL use a throwaway schema (`testdb.Schema`; in `internal/store`,
  `schemaStore(t, prefix)`). Tests in the shared schema scope their data with
  unique lane/key names and never assert global counts or truncate.
- Do not rely on test order across files, and prefer polling a condition to
  `time.Sleep`. A console test that depends on `Date.now` stubs it with a
  variable it advances (`mockImplementation(() => clock)`), never with a
  queue of `mockReturnValueOnce` values that React may consume.

## Web console (`web/console`)

- Reads go through `src/shared/http.ts`: `getJSON<T>(path)` rejects with
  `HttpError(status)` on non-2xx. Validate the payload shape in the caller and
  throw on a malformed one; never render it as empty.
- Board/task API calls and their types live in `src/shared/boardApi.ts` and
  `src/shared/boardTypes.ts`; an app's own endpoint types go in its `types.ts`.
- Polling uses `usePoll(load, ms)` from `src/shared/usePoll.ts` with a stable
  `load`; no `setInterval` loops and no hand-written `setTimeout` loops.
- localStorage goes through `readStored` / `writeStored` in
  `src/shared/storage.ts`; console-wide keys are in `STORAGE_KEYS`.
- Every entry calls `bootTheme()` before its first render.
- Navigation entries are data: a new page is one row in `destinations`
  (`aux`, `heading`, `alias` describe its place), never an index check.
- Keyboard shortcuts skip typing targets via `isTypingTarget`
  (`src/shared/search.ts`).
- Task states come from `src/shared/states.ts` (`STATE_GROUP_ORDER`,
  `stateLabel`); dates are formatted with `src/shared/time.ts`.
- Colors, spacing, type sizes, radii and fixed widths come from
  `src/design/tokens.css` (`var(--hk-*)`) whenever a token has the value.
- `npm run typecheck` must pass. After any change under `src`, run
  `npm run build` and commit `internal/ui/static/console` with it; CI fails on
  a stale bundle. Tests run with `TZ=Asia/Seoul` (set in `vitest.config.ts`).
