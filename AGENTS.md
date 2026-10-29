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
- `internal/testdb` — how tests reach Postgres.
- `tests/` — cross-package integration tests against a real server and DB.

## Checks

`scripts/check` runs CI's gates in CI's order (`scripts/check go`,
`scripts/check web` for one half). Run it before pushing.

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
- An error returned by the store or a service: `appErr(w, err)`.
- A new store sentinel the client must tell apart: declare
  `ErrX = errors.New("x_code")` in the store and add one row to `errorRules`.
  Set `reason: true` only when the text after `"x_code: "` is store-authored.
- Unmapped database/network failures become `500 internal_error` and are
  logged; plain validation errors become `400 invalid_context`. Do not add a
  catch-all that hides a 500 as a 400.
- Use `http.StatusX` constants, not numeric literals.

**Console errors** (`internal/ui`). `/ui/api/*` JSON routes answer with
`writeJSONError(w, status, code)` and `writeBoardJSON`; HTML and form routes
keep `http.Error` with a short text message.

**Request bodies.** Decode JSON with `decode(r, &v, max)` (or
`decodeLimited`): size-capped, unknown fields and trailing data rejected.

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
`errors.Is`. Wrap with `%w`, never `%v`. Go-side timestamps are
`time.Now().UTC()`. Every `ORDER BY` that feeds a `LIMIT` ends in a unique
column (usually `id`) so pages are deterministic.

**Migrations** (`internal/store/migration.go`). The comment there lists which
function owns each schema version. A new version:
1. takes the next number (`SchemaVersion + 1`) and bumps `SchemaVersion`;
2. runs through `migrateOnce(ctx, tx, N, func() error { return execAll(ctx, tx, vN) })`
   inside `Store.migrate`, with `INSERT INTO schema_version(version) VALUES (N)`
   as its last statement;
3. says in a comment why it is needed and what lock it takes.

**Remote client.** Branch on server error codes with
`remote.ErrorCode(err) == "not_found"`, never by comparing `err.Error()`.

**CLI.** Output is JSON via `printJSON` unless a command documents a text
form behind `--json`. Errors go to stderr; exit codes other than 1 are named
constants.

**Logging.** `log.Printf` with one `event key=value ...` line
(e.g. `api.internal_error err="..."`).

## Tests (Go)

- Reach Postgres only through `internal/testdb`: `testdb.URL(t)`,
  `testdb.Conn(t)`, or `testdb.Schema(t, prefix)` / `testdb.SchemaPool(t,
  prefix)`. Never read `HANDOFFKEEP_TEST_DB_URL` directly. Without it a test
  skips locally and fails in CI.
- Tests that count whole tables or run migration DDL use a throwaway schema
  (`testdb.Schema`). Tests in the shared schema scope their data with unique
  lane/key names and never assert global counts.
- Do not rely on test order across files, and prefer polling a condition to
  `time.Sleep`.

## Web console (`web/console`)

- Reads go through `src/shared/http.ts`: `getJSON<T>(path)` rejects with
  `HttpError(status)` on non-2xx. Validate the payload shape in the caller and
  throw on a malformed one; never render it as empty.
- Board/task API calls and their types live in `src/shared/boardApi.ts` and
  `src/shared/boardTypes.ts`; an app's own endpoint types go in its `types.ts`.
- Polling uses `usePoll(load, ms)` from `src/shared/usePoll.ts` with a stable
  `load`; no `setInterval` loops.
- Colors and spacing come from `src/design/tokens.css` (`var(--hk-*)`).
- `npm run typecheck` must pass. After any change under `src`, run
  `npm run build` and commit `internal/ui/static/console` with it; CI fails on
  a stale bundle. Tests run with `TZ=Asia/Seoul` (set in `vitest.config.ts`).
