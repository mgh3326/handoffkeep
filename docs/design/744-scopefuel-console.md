# #744 — scopefuel status page in the console, and server-owned pool policy

Status: design (docs only). Tier of this document: T2. No code ships with it.

Operator request (operator-desk, 2026-09-26): add a scopefuel page to
`console.robinco.dev/ui` next to `/ui/queue`. Phase 1 is read-only. Phase 2
adds operator control. Phase 2 cannot start until the pool policy's source of
truth moves from each host's `config.toml` to handoffkeep (hk).

Sources read for this design (repo paths; scopefuel at `origin/main` 9d6c120):

- hk console: `internal/ui/ui.go` (auth and method boundary), `internal/ui/api.go`
  (`/ui/api/*` routes), `internal/ui/catalog.go` (`/ui/grades` + read-only
  catalog BFF), `internal/ui/write.go` (CSRF and audit), `docs/ui.md`,
  `web/console/vite.config.ts` (one entry per React page).
- hk API/store: `internal/api/api.go` (bearer `Tokens`, `operatorClientID`,
  `/v1/bench/{reps,catalog,grades}`, `/v1/documents`), `internal/store/store.go`
  (`bench_reps`, `bench_catalog`, `documents`).
- scopefuel: `policy.py` (config.toml `[pools.<p>]`), `cache.py` (snapshot cache,
  TTLs, backoff), `model.py` (`Bucket`, `Scope`, `ProviderResult`),
  `quota_share.py` + `docs/quota-share.md` (the one existing cross-host quota
  path), `quota_v2*.py` (#578 shadow, account_ref), `grades.py` (#735
  propose/apply), `bench.py` (catalog read and freshness), `recommend.py` (the
  gate's use of `get_policy`/`get_boost`), `refresh.py`, `providers/codex.py`,
  `docs/catalog-server-mode.md`.

---

## 0. Invariants this design keeps

These hold in every phase. Each implementation task below is reviewed against
them.

1. **The page is read-only telemetry.** No host's gate reads another host's
   report from hk. The only cross-host quota input stays `quota_share` (claude,
   account-bound fingerprint, 15 min acceptance). Host reports are for display
   only. They are never an admission input.
2. **Pushing never measures.** A report is built from the local snapshot cache,
   the local policy and the local catalog view. It never calls a provider usage
   API, so building one cannot cause a 429.
3. **No secret reaches hk or the browser.** That covers tokens, OAuth
   material, account emails, account UUIDs, raw provider error bodies, local
   paths and hk or hub URLs. The ingest route enforces this with a whitelist,
   not by trusting the client (section 1.5).
4. **A down server never makes dispatch easier** (catalog-server-mode "widening
   fails closed"). Policy fetch failures can keep or tighten admission. They
   never loosen it (section 3.4).
5. **Nothing the UI does bypasses the gate.** There is no UI path to
   `--operator-request`, `consult_only` relaxation, or quota or cutoff skipping
   (#461).
6. **Every write is audited and reversible by a new write.** Audit rows are
   append-only. A revert is a new revision, never a delete.

---

## 1. Data flow: host → hk

### 1.1 How hosts reach hk today

- Transport: plaintext HTTP over Tailscale (`http://100.122.100.56:8800`),
  bearer token from `HANDOFFKEEP_URL`/`HANDOFFKEEP_TOKEN` or
  `~/.config/handoffkeep/config.env`. The token is sent over plaintext only
  after a per-use opt-in (#697: `allow_plaintext_catalog`,
  `allow_plaintext_quota_share`, `allow_plaintext_reps`).
- Tokens: `HANDOFFKEEP_AUTH_FILE` maps `HANDOFFKEEP_TOKEN_<client>` to secrets.
  `Tokens.Client` resolves the client id. `operator` is reserved for catalog
  writes (`403 operator_required`). Quota share runs on the shared bench token,
  which has **no per-key ACL**: any holder can forge `quota/...` documents
  (acknowledged in `docs/quota-share.md`).
- Browser: Cloudflare Access in front of `/ui`. The origin re-verifies the
  Access JWT and the email allowlist. `/ui/api/*` also admits allowlisted
  Access service tokens. Bearer tokens and Access assertions never substitute
  for each other.

### 1.2 What each host pushes: `scopefuel.host-report.v1`

One document per host, replaced on each push. Every field is either derived
from existing scopefuel structures or is an enum. The JSON below shows the
field set; exact names are fixed by the implementation task.

```json
{
  "schema": "scopefuel.host-report.v1",
  "host": "desktop",
  "scopefuel_version": "0.x.y+r3",
  "built_at": "2026-09-26T07:31:02Z",
  "cache_age_s": 41,
  "catalog": {
    "source": "server|cache|snapshot|unsupported|local",
    "stale": false,
    "digest": "a1b2c3d4e5f60718",
    "age_s": 812
  },
  "policy": {
    "mode": "local|shadow|server",
    "server_revision": 17,
    "server_fetched_age_s": 30,
    "effective": [
      {"pool": "codex", "class": "exclude", "class_origin": "local|server|builtin",
       "until": "2026-09-30", "boost": null, "boost_origin": null,
       "subscribed": true, "status": "expires 2026-09-30"}
    ],
    "drift": [{"pool": "codex", "field": "class", "local": "exclude", "server": "spend"}]
  },
  "pools": [
    {
      "pool": "codex",
      "status": "ok|stale|error|backoff|in_progress|disabled|unsubscribed",
      "source": "local|remote|operator",
      "fetched_age_s": 95,
      "error_kind": null,
      "http_status": null,
      "backoff_remaining_s": null,
      "account": {"key_kind": "account_ref|account_fp|host_local",
                  "key": "c0605596aa11bb22", "label": "My Org"},
      "plan": "pro",
      "buckets": [
        {"label": "5h", "window": "5h", "horizon": "now",
         "scope": {"kind": "account", "name": null},
         "used_pct": 42.0, "resets_at": "2026-09-26T09:00:00Z",
         "pace": 0.9, "severity": "ok"},
        {"label": "gpt-reserve 7d", "window": "7d", "horizon": "week",
         "scope": {"kind": "model", "name": "gpt-reserve"},
         "used_pct": 81.0, "resets_at": "2026-09-29T00:00:00Z",
         "pace": 1.3, "severity": "warn"}
      ]
    }
  ]
}
```

Field notes:

- **Buckets.** These are `Bucket.as_dict()` minus `note` and minus the
  `full_use_rate*` pair (display-only noise). `scope.kind` carries
  account, model or group, so model-scoped windows such as codex's
  per-rate-limit buckets (`Scope("model", name)` in `providers/codex.py`) come
  through unchanged. The report does not invent windows. Whatever the provider
  measured is what is shown.
- **Measurement errors.** Only the enum `error_kind` (the #576 vocabulary:
  `rate_limited|server|network|auth|credentials|http|unknown`), `http_status`
  and `backoff_remaining_s`. The `error`, `warning`, `last_error` and `hint`
  strings are **not** sent: they are free text built from provider responses
  and are the likeliest place for a token, email or URL to leak.
- **Account key.** The key is chosen in this order:
  1. quota v2 `account_ref` (`acct_…`, operator-enrolled) when a binding is
     valid;
  2. `account_fp` when `account_fp_kind == "account"`;
  3. otherwise `host_local`: the key is omitted and the UI treats the row as
     "this host's login, identity unproven".

  A token-hash fingerprint (`account_fp_kind == "token"`) is never sent, the
  same #659/N-1 rule `quota_share` applies. `label` is `safe_label()` output
  only (no `@`, max 32 characters). `session_fp` is not sent (it is
  provenance for quota share, not needed for display).
- **Catalog snapshot id.** `digest` = sha256 over the host's sorted catalog
  rows (the same row serialisation `grades._input_digest` uses), truncated to
  16 hex. hk computes the same digest over its live `bench_catalog`. The page
  can then say "host on current catalog" or "host behind", without the host
  shipping the catalog.
- **Policy in effect.** The value the gate would use right now per pool, where
  it came from, and, in shadow mode, where local and server disagree (section
  3.5). `plan`, `price_usd` and `capacity_weight` are not pushed in v1.
- **Not in the report:** tokens, OAuth fields, emails, account UUIDs,
  `session_fp`, config or cache paths, hk or hub URLs, provider error text,
  manual self-report `reason` text, reps, the full catalog.

Size cap: 64 KiB per report (today there are 6–9 pools with at most a few
buckets each, which is under 8 KiB).

### 1.3 Cadence

- **On change:** after a successful or failed `collect`/`refresh` that
  modified the cache entry, push at most once per 60 s per host (coalesced
  with a lock file next to `snapshots.lock`). This is a side effect, like the
  `quota_share` publish, and it is fail-open: a push failure never changes
  collect's result or exit code.
- **Heartbeat:** a user timer (systemd `--user` on Linux hosts, launchd on
  macOS) runs `scopefuel report push` every **5 min**. It reads the cache
  only (invariant 2). Without it, an idle host would look dead, because
  `refresh` is event-driven through herdr hooks.
- **Kill switch:** `SCOPEFUEL_HOST_REPORT=off` disables both paths (same shape
  as `SCOPEFUEL_QUOTA_SHARE`).

### 1.4 Identity and auth per host

- New ingest route: `PUT /v1/scopefuel/hosts/{host}/report`, bearer auth.
- **Per-host client id (recommended):** `HANDOFFKEEP_TOKEN_scopefuel-<host>`
  entries in the auth file. The server requires `client == "scopefuel-" +
  {host}`, so a host can only write its own row. This closes, for reports, the
  "shared token can forge" gap that quota share accepts.
- **Transition:** until per-host tokens are provisioned, the shared bench
  client id is accepted too. The row is then stored with
  `auth = "shared"` and the page shows the host as *identity unverified*. A
  flag, `HANDOFFKEEP_SCOPEFUEL_REPORT_SHARED=0`, turns the shared path off
  once every expected host has its own token.
- `{host}` matches `[a-z0-9][a-z0-9._-]{0,62}` (the quota v2 machine pattern).
  The body's `host` must equal the path.
- Plaintext: a new per-use opt-in `allow_plaintext_host_report` (and
  `allow_plaintext_policy` for section 3), following #697. Moving hk to
  `tailscale serve --https` makes both unnecessary and stays the preferred
  fix (open question Q11).
- Reads by the browser go through `/ui/api/scopefuel/*` under the existing
  Access session. Service identities are refused on these routes, as for
  `/ui/api/bench/catalog`, because provenance and account labels are operator
  reading material.

### 1.5 Ingest validation (server side, fail-closed)

- `DisallowUnknownFields` at every level. The schema version is exact.
- Every string: a length cap, a character class per field (enums are
  enumerated; `label` rejects `@`, quotes and control characters), and
  `guard.Reject`. **`guard` must first learn the Claude OAuth token prefixes**
  (`sk-ant-oat01-`, `sk-ant-ort01-`), which `docs/quota-share.md` records as
  missed today. That is task T-1 below and a prerequisite for ingest.
- Numbers must be finite. `used_pct` must be in [0,100] or null.
  `resets_at` must be RFC 3339.
- A rejected report returns 400 with a field path, never an echo of the
  value, and is audit-logged with host and reason only.

### 1.6 Storage

- `scopefuel_host_reports(host PK, auth, body JSONB, received_at,
  report_built_at)`. The row is upserted and stores the latest report only.
  `received_at` is the server clock and the authority for staleness.
- History is not stored in Phase 1 (open question Q12). A later history table
  could also feed fleet-metrics' "eligibility snapshot" coverage gap
  (`docs/fleet-metrics.md`), but that is out of scope here.

### 1.7 Read from hk directly (no host push)

| Data | Source | Notes |
|---|---|---|
| Reps | `bench_reps` (`ListBenchReps`) | newest first, limit 50 on the page |
| Catalog | `bench_catalog` (`ListBenchCatalog`) | same rows as `/ui/grades` |
| Grades proposal | document `scopefuel/grades/proposal/latest` | see below |
| Server policy | `scopefuel_pool_policy` (section 3) | from Phase 1b |

**Grades proposal.** `grades propose` is Python and evaluates the #735 rule
client side. hk does **not** re-implement the rule, because two
implementations would drift. A designated host (default: desktop) runs
`scopefuel grades propose --json` daily, and also on demand from the CLI, then
PUTs the artifact as a `note` document under that key. The artifact is
`proposal_to_json` unchanged: rule_version, min_passes, digest,
params.cli_exclusions, reps backend summary, `window_incomplete`, per-rung
results with evidence refs. It carries no secrets. It uses `host` only as a
provenance string.

hk adds a derived **freshness check** at read time: the proposal is
*superseded* when `max(bench_reps.id)` or the catalog digest has changed since
the artifact's `generated_at`. The digest itself cannot be recomputed in Go, so
the page says "evidence changed since proposal; rerun propose" rather than
claiming the digest is invalid.

---

## 2. Phase 1 page: `/ui/scopefuel`

A new React entry, `scopefuel`, in `web/console/vite.config.ts`, shaped like
`grades`/`deploys`. The Go template is a mount point only. There is no CSRF
token in Phase 1 because there is no write form. The console CSP is set as
for `/ui/grades`. A nav link is added to every page shell.

BFF routes (all GET, `no-store`, service identities refused):

- `/ui/api/scopefuel/overview`: hosts, quota matrix, policy view and catalog
  digest, in one response so panels agree on one `generated_at`.
- `/ui/api/scopefuel/reps?limit=50&profile=&grade=&effort=`
- `/ui/api/scopefuel/proposal`
- The catalog reuses `/ui/api/bench/catalog`.

Polling is every 30 s, non-overlapping, and keeps the last good data on
failure (the queue board pattern). Reports arrive every 5 min at most, so
faster polling buys nothing.

### 2.1 Panels and fields

**A. Fleet strip** (top, one line per fact)

- Hosts reporting: `fresh n / expected N`, plus any names never seen.
- Policy: server revision `r17`, "`k` hosts on r17, `m` behind, `j` in local
  mode". Before section 3 ships this reads "policy: per-host (config.toml)".
- Catalog: "`k` hosts on current catalog", with any host on `snapshot`
  (stale) called out in red, as the catalog doc requires ("disclosure is the
  precondition").
- Proposal: age, digest, and whether it is superseded.

**B. Quota by pool and account** (main panel)

- Rows are grouped by pool. Within a pool there is one row per account key
  (`account_ref` > `account_fp`), displayed as `fp8 (label)` exactly like
  scopefuel's account tag. `host_local` accounts get one row per host, marked
  "identity unproven".
- Columns: the account-scope buckets ordered by window length (5h, then 7d),
  then **model-scoped and group-scoped buckets as indented sub-rows** under
  the account row (for example `codex · model gpt-reserve · 7d`).
- Cell: `used_pct` bar with severity colour, `resets in 3h12m` (relative, with
  the absolute time on hover), pace, and a provenance line
  `desktop · 2m ago · local|remote|self-reported (unverified)`.
- When several hosts measured the same account, the cell uses the most recent
  successful measurement. A "3 hosts" chip expands to per-host values, and
  disagreements over 5 pct-points are flagged. Such disagreements are usually
  measurement lag, sometimes a wrong account binding.
- A pool whose effective class is `exclude` or `unsubscribed` on *any* host is
  badged, with the hosts named.

**C. Host measurement status** (grid: host × pool)

- Host header: `received_at` age, scopefuel version, `auth`
  (per-host/shared-unverified), catalog source and digest match, policy mode
  and revision, drift count, and clock skew (`built_at` vs `received_at`, shown
  when over 2 min).
- Cell: status enum, `error_kind`/`http_status`, backoff remaining, last
  success age, and source (local, remote or self-reported).
- Expected hosts come from `HANDOFFKEEP_UI_SCOPEFUEL_HOSTS` (default
  `desktop,m1b,pi,mac-personal`). Reporting hosts not in the list (m1, ncp)
  are shown after them, so a newly reporting host appears without a config
  change, and a silent expected host appears as *never reported* instead of
  vanishing.

**D. Catalog** (reuse the grades table component)

- profile, effort, model_id, pool, grade, gate (`default|escalation|consult_only`),
  retired, decided_by/at, deviation_ref, plus one derived column
  **admission now**: `ok`, `pool excluded (hosts…)`, `pool unsubscribed`,
  `consult_only`, or `retired`. This column is derived from host reports and
  server policy for display. It is not a gate call.

**E. Policy**

- Before section 3 exists: the per-host *effective* policy from reports, one
  row per pool, with columns per host. Cells that differ across hosts are
  highlighted, which is today's hand-applied state made visible.
- After section 3: server policy rows (class, boost, until, subscribed,
  decided_by, reason, revision, updated_at), the audit tail (last 20 events),
  and per-host drift from shadow mode.
- Expired entries are shown struck through with "expired", mirroring
  `policy list`, so a lapsed tweak stays visible.

**F. Recent reps**

- id, recorded_at, profile, effort, model_id, task_ref (a `hk:task/<id>`
  reference links to `/ui/tasks/<id>`; everything else is text), tier, role,
  grade, rounds, blockers_found, completed, created_by (the client id), and
  notes truncated to 160 characters with the full text on expand (text only,
  as React already escapes).

**G. Grades proposal**

- Header: generated_at age, host, rule_version, min_passes, digest,
  catalog_source, reps backend and `window_incomplete` (a warning when true),
  and the superseded state.
- Rows: rung, current grade → target, action
  (`promote|demote|conflicted|hold`), evidence refs, and counted and uncounted
  passes and fails. Conflicted rows show both sides. `unrung` evidence is
  collapsed under a toggle.

### 2.2 Empty, stale and error states

| Situation | Display |
|---|---|
| Host report age ≤ 10 min | normal |
| 10 min < age ≤ 60 min | amber "report 23m old"; values shown dimmed |
| age > 60 min | red "stale report"; quota cells hidden behind a "show last known" toggle so an old value is never read as current |
| Expected host with no row | "never reported", with a short setup hint (timer and opt-in) |
| Pool `fetched_age_s` older than 2 × the scopefuel provider TTL | cell amber "measured 25m ago" even if the report is fresh |
| Pool status `error` | enum and HTTP status only (for example `auth · 401`); never a message body |
| Pool status `backoff` | "backoff 7m left", with the last known value dimmed |
| `unsubscribed` | grey, no values, "not measured (unsubscribed)" |
| Catalog `snapshot` on a host | red "catalog=stale" chip on that host |
| No proposal document | "no proposal published yet", with the command that publishes one |
| Proposal superseded | amber, "evidence changed since proposal" |
| BFF upstream error | the panel keeps its last good data plus an error ribbon; other panels are unaffected |
| No reps | "no reps recorded" |

### 2.3 What is never rendered

Tokens or any bearer material, CSRF values, account emails, account UUIDs, full
fingerprints (only fp8), `session_fp`, provider error text, hk or hub URLs,
local paths, and manual self-report reasons. The label is the existing safe
display name only. The BFF builds its response from typed structs, never by
passing stored JSON through, so a field the ingest validator missed still
cannot reach the browser.

---

## 3. Policy source of truth on the server

### 3.1 Scope of "policy"

Today `[pools.<p>]` in `~/.config/scopefuel/config.toml` holds `class`, `until`,
`note`, `boost` (pool-level; boost shares `until`), `cutoff`, `on_exhaust`,
`plan`, `price_usd` and `capacity_weight`. The keys are pools (provider ids),
not profiles.

Profile-level restriction already has a server home: the catalog's `gate` and
`retired_at`. So server policy stays **pool-level** (open question Q5).

The fields move in two steps:

| Step | Fields | Effect on admission |
|---|---|---|
| P-a | `class`, `until`, `note→reason`, `boost`, new `subscribed` | class/subscribed: yes (T3); boost: ranking only |
| P-b (later) | `cutoff`, `on_exhaust` | yes (T3) |
| stays local for now | `plan`, `price_usd`, `capacity_weight`, `[settings]`, `[bench]` | ranking or host configuration |

The existing `/ui/api/policy/active` (the policy *manifest* document pointer)
is unrelated. Everything here is named "pool policy" and lives under
`scopefuel` routes, to avoid the collision.

### 3.2 Schema

```
scopefuel_pool_policy
  pool          TEXT PRIMARY KEY        -- provider id, [a-z][a-z0-9._-]{0,63}
  class         TEXT NULL CHECK (class IN ('preserve','spend','exclude'))
  until         DATE NULL               -- UTC date, inclusive (expired when until < today)
  boost         INTEGER NULL
  subscribed    BOOLEAN NOT NULL DEFAULT TRUE
  cutoff        DOUBLE PRECISION NULL   -- P-b; NULL = host default
  on_exhaust    TEXT NULL               -- P-b
  reason        TEXT NOT NULL           -- required, 8..500 bytes, guard-checked
  decided_by    TEXT NOT NULL           -- 'operator(web) <email>' | 'cli:<client>' | 'migration'
  revision      BIGINT NOT NULL         -- global revision at which this row last changed
  updated_at    TIMESTAMPTZ NOT NULL

scopefuel_policy_meta
  revision      BIGINT NOT NULL         -- single row; bumped on every write

scopefuel_policy_events                 -- append-only audit
  id BIGSERIAL, revision BIGINT, pool TEXT, action TEXT
  ('set','clear','subscribe','unsubscribe','import','revert'),
  before JSONB, after JSONB, reason TEXT, decided_by TEXT,
  source TEXT ('ui','cli','migration'), confirm_ref TEXT NULL, at TIMESTAMPTZ
```

The validation rules are identical to `policy.py`, and a shared JSON case file
is tested in both repos (task T-6):

- `class` requires `until`.
- `boost` must be an int, not a bool, and requires `until`.
- A boost-only row may omit `class` (the provider builtin is inherited, as in
  `_active_override`).
- `subscribed = false` does not take `until`. It lasts until the operator
  resubscribes.
- `clear` deletes the row (recorded in events with `before`).

There is no per-host override column (open question Q4). If per-host overrides
are wanted later, they become a `host` key column with `*` as the fleet row.

### 3.3 API

- `GET /v1/scopefuel/policy` with any bearer client. It returns `{revision,
  generated_at, pools:[…]}` without `decided_by` emails (only
  `decided_by_kind`), with `ETag: "r<revision>"`, and answers
  `If-None-Match` with 304.
- `PUT /v1/scopefuel/policy/{pool}` and `DELETE …/{pool}` require the
  `operator` client (CLI path). The request needs `If-Match: "r<revision>"`
  (409 on mismatch, so no lost update) and a `reason`.
- `POST /v1/scopefuel/policy/import` requires `operator`. It is a one-time
  migration seed, all-or-nothing, and refused when revision > 0 unless
  `--replace` is passed with the current revision.
- The UI write routes are in section 4. They write through the same store
  function, so validation and audit are identical.

### 3.4 How hosts fetch and cache

- `scopefuel` fetches the policy at most once per 60 s (TTL) during
  `collect`/gate/`policy launch`. It uses a conditional GET and a 3 s timeout.
- The cache is `~/.cache/scopefuel/policy-server.json` (0600) with
  `{revision, fetched_at, pools}`, written atomically under the existing cache
  lock.
- **Offline rule (fail-degraded-closed):**
  - Restrictive entries from the last known server policy (`class=exclude`,
    `subscribed=false`, and in P-b a lower `cutoff`) are honoured for as long
    as their own `until` says, **regardless of cache age**. An outage never
    lifts an exclusion early.
  - Permissive entries (`class=spend`, `boost`, and in P-b a raised `cutoff` or
    `on_exhaust`) are honoured while the cache is younger than
    `policy_stale_max_s` (default 24 h, matching `catalog_stale_max_s`). After
    that they drop to builtin, labelled `policy=stale`.
  - No cache at all in server mode: builtin classes, labelled
    `policy=unavailable`. The gate proceeds for `default`-gate profiles, as for
    `catalog=stale`.
  - Every gate line and `--json` output carries `policy.source`
    (`server|cache|stale|local`) and the revision. That is the disclosure
    precondition.

### 3.5 Precedence during migration

A new `[policy] source` key (env override `SCOPEFUEL_POLICY_SOURCE`) sets the
mode:

| Mode | Gate uses | Server fetched | Notes |
|---|---|---|---|
| `local` (today, default until switch) | config.toml | no | unchanged behaviour |
| `shadow` | config.toml | yes | report carries `drift[]`; no admission change |
| `server` | merge (below) | yes | config.toml policy fields become tighten-only |

The `server` merge, per pool:

- **class:** the more restrictive of server and local (`exclude` beats
  `preserve` beats `spend`). A local `exclude` still works as an emergency
  brake on one host while offline. A local `spend` or boost can **never**
  widen server policy.
- **subscribed:** false if either side says false. Locally, "unsubscribed"
  has no config.toml spelling in v1; the server is the only writer.
- **boost:** server only. A local boost is ignored with a one-line warning.
- Fields not yet on the server (P-b and the local-only fields) keep reading
  config.toml.

The CLI in `server` mode: `scopefuel policy set/clear` writes to hk, requires
`HANDOFFKEEP_TOKEN` to be the operator credential and `--reason`, and prints
the new revision. Without the operator token it refuses and says so. It never
silently writes config.toml. `policy set --local` remains for an explicit
tighten-only local brake. `policy list` shows server, local and effective
columns.

### 3.6 Switch-over plan

1. **Ship the server side (T-7)** with no reader.
2. **Ship scopefuel with `shadow` (T-8)**, default still `local`. Put hosts in
   `shadow`. Nothing changes at the gate.
3. **Seed (T-9, operator action).** `scopefuel policy export --json` runs on
   desktop, m1b, pi and mac-personal (plus m1 and ncp if they run scopefuel).
   A reconcile script lists per-pool differences. The operator picks one value
   per pool (Q14 default: the most restrictive, with the latest `until`). The
   result goes to `POST /v1/scopefuel/policy/import` with
   `decided_by=migration` and reason `hk:task/744 seed`.
4. **Parity window.** Every host in `shadow` shows `drift = 0` on the page for
   24 h. Any drift is resolved by editing server policy or local config, not
   by flipping.
5. **Canary flip (T-10, T3).** Flip `pi` to `server`, then m1b. Watch the
   gate lines for `policy.source=server` and the unchanged admission set on
   the page for 24 h. Then flip desktop and mac-personal.
6. **Clear local brakes.** Once all hosts run `server`, `scopefuel policy
   migrate --clear-local` removes class, until, note and boost from each host's
   config.toml after backing it up to `config.toml.pre-744`. Leftover local
   `exclude`s would otherwise keep tightening forever, invisibly.
7. **Retire (T-13, T3, later).** `local` mode is removed only after 30 days
   without a rollback.

**Rollback** for any host at any step: set `SCOPEFUEL_POLICY_SOURCE=local`
(or `[policy] source = "local"`). Step 6 keeps a backup, so rolling back after
step 6 is `cp config.toml.pre-744 config.toml` plus the env var. Server-side
rollback of a bad edit is a `revert` to revision N, written as a new revision
with its own audit row.

### 3.7 Audit

- Every write, from any source, adds one `scopefuel_policy_events` row in the
  same transaction as the policy change.
- The UI route also emits one server audit line (the existing `h.audit`
  shape: email, action, target=pool, event_id=revision, result). CSRF values
  and reasons are never logged in that line.
- On every revision the server emits a `lane.event` to `operator-desk`:
  `[scopefuel-policy] r18 codex class exclude until 2026-09-30 by operator(web)`.
  Operator-desk and the other lanes can then see a policy change without
  polling. The emit fails open (the policy write stands) and is reported on
  the page if it fails.

---

## 4. Phase 2: operator control

### 4.1 Role and request protection

- **Operator role:** a new `HANDOFFKEEP_UI_OPERATOR_EMAILS`, a subset of
  `HANDOFFKEEP_UI_ALLOWED_EMAILS`. It is empty by default, and **empty means
  every scopefuel write is disabled** (the buttons are not rendered and direct
  POSTs are refused with 403), the same fail-closed shape as compose without
  lanes. Non-operator emails keep the Phase 1 read view.
- **Routes live outside `/ui/api/`** (`POST /ui/scopefuel/policy/...`), as task
  comments and dispositions do. `ServeHTTP` then refuses Access service
  identities structurally, and each handler also requires
  `identity.Email != ""` and the operator allowlist.
- **CSRF:** the existing email-bound, 12 h `hk_ui_csrf` token plus the
  same-host `Origin` check (`Referer` only when `Origin` is absent). A failed
  check performs no store write and no hub emit. The React page reads the token
  from a `<meta name="hk-csrf">` in its mount template and posts
  form-encoded bodies (`ParseForm` → `validCSRF`), exactly as task comments
  do today (`internal/ui/comments.go`). Unknown form fields are refused.

### 4.2 Actions

| Action | Route | Preconditions | Tier of the capability |
|---|---|---|---|
| Set class preserve/spend/exclude with until | `POST /ui/scopefuel/policy/set` | server mode shipped (T-10) | T3 |
| Set/clear boost with until | same | same | T3 (ranking on every host) |
| Clear a pool row | `POST /ui/scopefuel/policy/clear` | same | T3 |
| Unsubscribe / resubscribe | `POST /ui/scopefuel/policy/subscription` | T-12 semantics shipped | T3 |
| Revert to revision N | `POST /ui/scopefuel/policy/revert` | — | T3 |
| Approve grades apply for proposal digest D | `POST /ui/scopefuel/grades/approve` | #741 degraded-input refusal merged; proposal not superseded | T3 |

**Confirmation flow**, for every action:

1. The page sends `POST …/preview` with the intended change. The server
   returns the diff (`before → after`) and the **projected impact**: for a
   class, subscription or cutoff change, the catalog rungs whose "admission
   now" column would change and the hosts currently reporting that pool. This
   is computed from the latest reports and the catalog, and labelled
   "projection, not a gate call".
2. The operator enters a reason (required, 8–500 bytes) and, for `exclude`,
   `unsubscribe` and `approve`, **types the pool name or digest** to confirm.
3. `POST …/set` carries `If-Match: r<revision>` from the preview. A 409 means
   someone else changed the policy, and the page re-previews.
4. The result shows the new revision and the hosts that have not yet fetched
   it, with a countdown from their last fetch plus the 60 s TTL.

Bounds (Q7):

- `until` may be at most 30 days ahead for class and boost.
- `boost` must be within [-100, 100].
- `unsubscribe` has no `until`.

**Grades apply, with no operator token in the browser or in hk's UI process:**

- "Approve" records `{digest, decided_by=operator(web) <email>, reason,
  deviation_ref}` as an approval document and emits a
  `[decision] grades-apply <digest>` lane event to operator-desk.
- The designated runner host, which already holds the operator credential used
  for `push-catalog` today (Q10), runs `scopefuel grades apply --approval
  <doc>`. That command:
  - re-derives the proposal and requires the digest to match (existing
    `apply_proposals` behaviour);
  - requires #741's degraded-input refusal to pass (reps backend complete, no
    `window_incomplete`, catalog `source=server`);
  - PUTs the catalog with `decided_by` taken from the approval;
  - writes the outcome back to the approval document.
- The page shows `approved → applied|refused(<reason>)`.
- Until #741 is merged, the Approve button is not rendered. The route also
  refuses with `409 degraded_input_guard_missing`, keyed off a scopefuel
  capability flag in the proposal artifact, so an old runner cannot apply it.

### 4.3 Never from the UI

- Editing or viewing tokens, OAuth credentials, `config.env`, or `config.toml`
  as text.
- Logging a host into or out of a provider, re-login, or token refresh (#616
  stays manual).
- `--operator-request`, relaxing `consult_only`, or skipping quota, cutoff or
  exclude checks (#461).
- Free-form catalog edits (model_id, pool, gate, retire, new rows). Catalog
  writes stay on the operator-token `PUT /v1/bench/catalog`. The UI only
  approves a verified proposal digest.
- Changing the grades rule, `min_passes`, or the supersedes exclusions.
- Editing, deleting or adding reps.
- Manual quota self-reports (`scopefuel manual`), which stay a host-local
  operator act.
- Triggering a provider measurement on a host (it would multiply usage-API
  calls, the #653 429 cause).
- Deleting audit events or policy history.
- Any remote command execution on a host.

---

## 5. Implementation tasks

Tiers: **T3** = changes what the gate admits (or how it ranks) on every host.
**T2** = a new API or page, or a cross-repo change with no admission effect.
**T1** = small and local.

| # | Task | Repo | Tier | Depends on |
|---|---|---|---|---|
| T-1 | `guard`: add Claude OAuth token prefixes (`sk-ant-oat01-`, `sk-ant-ort01-`) and tests | hk | T1 | — |
| T-2 | Host report ingest: schema, validator, `scopefuel_host_reports`, per-host client binding plus the shared-token transition flag, and docs | hk | T2 | T-1 |
| T-3 | `scopefuel report build/push`, the change-triggered push in collect/refresh, systemd and launchd timer units, `allow_plaintext_host_report`, and kill switch | scopefuel | T2 | T-2 |
| T-4 | BFF `/ui/api/scopefuel/{overview,reps,proposal}` plus the catalog digest in Go (shared digest fixture with scopefuel) | hk | T2 | T-2 |
| T-5 | `/ui/scopefuel` React entry: panels A–G, states table, nav link, vitest | hk | T2 | T-4 |
| T-5b | Designated-host daily `grades propose --json` publish to `scopefuel/grades/proposal/latest` | scopefuel | T1 | — |
| T-6 | Pool-policy validation contract: shared JSON case file, tested in both repos | both | T1 | — |
| T-7 | Server pool policy: tables, `GET/PUT/DELETE/import` routes (operator for writes), revision/ETag/If-Match, events, lane-event emit, docs | hk | T2 (no reader yet) | T-6 |
| T-8 | scopefuel `[policy] source` with `local` and `shadow`: fetch, cache, `drift` in report, `policy export --json` | scopefuel | T2 (no admission change) | T-3, T-7 |
| T-9 | Seed: export on each host, reconcile, operator import | ops | T2 | T-8 |
| T-10 | scopefuel `server` mode: restrictive merge, offline rule, `policy.source` disclosure, CLI writes to hk, then host-by-host flip | scopefuel + ops | **T3** | T-9 plus 24 h zero drift |
| T-11 | Phase 2 UI writes: operator allowlist, CSRF routes, preview/confirm, revert, audit | hk | **T3** | T-10 |
| T-12 | `subscribed=false` semantics: gate treats it as exclude, collect skips measuring that pool, report status `unsubscribed` | scopefuel (+ hk UI toggle) | **T3** | T-10 |
| T-13 | `policy migrate --clear-local`, then retire `local` mode after 30 days | scopefuel | **T3** | T-10 plus 30 days |
| T-14 | Grades approve route plus runner `grades apply --approval` | hk + scopefuel | **T3** | #741, T-11, T-5b |
| T-15 | P-b: `cutoff`/`on_exhaust` to server policy | both | **T3** | T-10 |

Order: Phase 1 is T-1 → T-2 → (T-3 ∥ T-4) → T-5. T-5b is independent. The
policy line is T-6 → T-7 → T-8 → T-9 → T-10 → (T-11, T-12, T-13) → T-15.
T-14 is last. Phase 1 (T-1..T-5b) delivers the operator's read request
without touching any gate. Panel E then shows per-host policy differences
before server policy exists.

---

## 6. Open questions for the operator (each has a recommended default)

| # | Question | Recommended default |
|---|---|---|
| Q1 | Per-host hk client ids for reports, or the shared bench token? | Per-host `scopefuel-<host>` ids. The shared token is accepted only during Phase 1 and shown as "identity unverified". |
| Q2 | Push cadence | On change (at most once per 60 s) plus a 5 min cache-only heartbeat timer. |
| Q3 | Expected host list | `desktop,m1b,pi,mac-personal`. m1 and ncp appear automatically if they report. |
| Q4 | Per-host policy overrides on the server? | No. Fleet-wide rows only. A local tighten-only brake covers emergencies. |
| Q5 | Profile-level policy? | No. Profile restriction stays in the catalog (`gate`, `retired_at`). |
| Q6 | Meaning of "unsubscribed" | Durable, no `until`. The gate treats it as exclude, the pool is not measured (no 401/429 noise), and it shows grey. Resubscribing is an operator write. |
| Q7 | Bounds on UI edits | `until` at most 30 days ahead, boost within [-100,100], a reason is always required, and a typed confirmation for exclude, unsubscribe and approve. |
| Q8 | Offline window for *permissive* server policy | 24 h (`policy_stale_max_s`, same as the catalog). Restrictive entries persist to their own `until`. |
| Q9 | Who is an operator in the UI? | The new `HANDOFFKEEP_UI_OPERATOR_EMAILS`. Empty means writes are disabled. |
| Q10 | Grades apply runner host | The host that runs `push-catalog` with the operator token today. The token never enters hk's UI process. |
| Q11 | Plaintext opt-ins vs https first | Ship with per-use opt-ins now (#697 pattern). Move hk to `tailscale serve --https` as a separate task, after which the opt-ins lapse. |
| Q12 | Keep report history? | No, latest only in Phase 1. Revisit with fleet-metrics' eligibility gap. |
| Q13 | Should the UI trigger a measurement refresh? | No, never (#653). |
| Q14 | Seed conflict rule when hosts disagree | The most restrictive class and the latest `until`, listed for the operator to confirm before import. |
| Q15 | Lane-event notification on policy change | Yes, to operator-desk, fail-open. |
