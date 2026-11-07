# MCP for ChatGPT (`/mcp`)

hk serves the MCP streamable HTTP transport at `/mcp`. ChatGPT connects to
`https://hk-mcp.robinco.dev/mcp` through Cloudflare Access with Managed OAuth,
so the operator signs in once and the origin receives a signed Access user
assertion. The existing bearer-token clients keep using the same route.

## Proxied hosts

cloudflared reaches the origin over loopback (`127.0.0.1:8800`) and forwards
the public `Host`, for example `hk-mcp.robinco.dev`. The MCP SDK's DNS
rebinding guard rejects a loopback request whose `Host` is not loopback, which
is meant for unauthenticated localhost servers; `/mcp` authenticates every
request before the SDK transport sees it, so `HTTPHandler` disables that guard
and a proxied request is decided by its credential alone. The temporary
cloudflared `httpHostHeader: localhost` workaround on the `hk-mcp` route is
therefore unnecessary once this build is deployed and can be removed.

## Credentials

A request authenticates with either credential:

- **hk bearer token**, unchanged: `Authorization: Bearer <token>`. The client
  id names the principal as before.
- **Cloudflare Access email assertion**: `Cf-Access-Jwt-Assertion` for the MCP
  application's audience. The origin verifies the signature against the team
  JWKS, requires RS256, issuer, audience, expiration and an allowlisted email.

Access **service-token** identities are not accepted on this path: they have no
email, and `/mcp` is an operator surface. The existing service-token path on
the `hk-api` hostname is untouched.

Any failure answers `401` with the body `{"error":"unauthorized"}` and
`WWW-Authenticate: Bearer`. hk does not serve OAuth protected-resource
metadata; Cloudflare's edge serves the OAuth discovery document for this
hostname.

## Configuration

The Access credential is mounted only when all three settings are nonempty
after trimming whitespace. Incomplete configuration disables that credential
and leaves the bearer path working, the same fail-closed rule `/ui` follows.
Startup logs the state without the configured values:

```text
mcp.access enabled
mcp.access disabled reason=incomplete_config team_domain_set=false aud_set=true allowlist_set=false
```

| Environment variable | Purpose |
| --- | --- |
| `HANDOFFKEEP_MCP_CF_TEAM_DOMAIN` | Cloudflare Access team domain, for example `example.cloudflareaccess.com` |
| `HANDOFFKEEP_MCP_CF_AUD` | Access application audience tag for the MCP hostname |
| `HANDOFFKEEP_MCP_ALLOWED_EMAILS` | Comma-separated email allowlist admitted on `/mcp` |

The JWKS endpoint is derived as
`https://<team-domain>/cdn-cgi/access/certs`. RSA keys are cached for ten
minutes, and an unknown key ID triggers at most one refresh per minute, using
the same verifier as `/ui`.

## Cloudflare setup

The design session creates this after the change lands. hk only reads the
assertion.

1. Create a Cloudflare Access application for `hk-mcp.robinco.dev` and point
   the tunnel route at the existing hk server (`/mcp`). Leave `hk-api` and its
   service-token policy alone.
2. Add an Allow policy for the operator's email only.
3. Turn on **Managed OAuth** for the application. Access acts as the OAuth
   authorization server, including PKCE S256 and dynamic client registration /
   client-id metadata document handling, and issues the user JWT that reaches
   the origin.
4. Allow these redirect URIs:
   - `https://chatgpt.com/connector_platform_oauth_redirect`
   - `https://chatgpt.com/connector/oauth/*`
5. Set the three `HANDOFFKEEP_MCP_*` variables on the server and restart.

No Worker OAuth proxy is needed, and hk serves no authorization-server or
protected-resource metadata itself.

## Identity and attribution

An Access identity becomes the principal `operator:<email>` with channel
`chatgpt`, so works written from ChatGPT are attributed like console writes:

- `task_comment` stores `operator:<email>` as the comment author.
- `decision_answer` closes the request on the verified operator path with
  actor `operator:<email>` and provenance `operator_direct`.
- `task_intake` records `source=chatgpt` and a store-name-safe form of the
  principal as `created_by` (the store's intake path accepts names only); the
  tool response reports the exact `operator:<email>` principal and the
  `chatgpt` channel.

The store's decision channel column has no `chatgpt` value and this change
adds no migration, so a verified Access answer is recorded on the existing
`console` channel and a bearer answer on the existing `cli` channel. Human-only
requests therefore stay open for a bearer client and close only through an
Access identity, matching the console and CLI rules.

## Tools

Read tools set `readOnlyHint: true`. Write tools set `readOnlyHint: false` and
`destructiveHint: false`: they add records and never change task state,
priority or lane.

| Tool | Kind | Arguments | Result |
| --- | --- | --- | --- |
| `task_search` | read | `query`, optional `state`/`lane`/`project`, `limit` ≤ 50 | Task summaries from the console search path, including Korean translations |
| `task_show` | read | `id` | Title (with `title_ko` when present), state, priority, lane, project, `refs.pr`, last 20 comments, last 10 events |
| `task_get` | read | `id` | One task in full: body, state, priority, kind, lane, project, area, labels, `claimed_by`, `refs`, parent, children, relations and blockers, `project_owner`, translations, YouTrack link, timestamps, `console_url`, `field_sources`; comments and events are a five-item preview |
| `task_list` | read | optional `state`/`lane`/`project`, `limit` ≤ 100, `cursor`, `updated_since` | Task summaries; paged calls also carry `next_cursor` |
| `task_comments` | read | `id`, optional `cursor`, `limit` ≤ 100 | One page of comments, oldest first, with `next_cursor` and `has_more` |
| `task_events` | read | `id`, optional `cursor`, `limit` ≤ 100 | One page of events, oldest first, with `next_cursor` and `has_more` |
| `changes_since` | read | optional `cursor` or `since`, `limit` ≤ 100 | One page of the change feed: `items`, `next_cursor`, `max_cursor`, `has_more` |
| `decisions_pending` | read | none | Open structured requests with question, options, `question_ko`/`options_ko` when fresh, and state |
| `decision_answer` | write | `request_id`, `option` or `text`, optional `note` | Store result; an already answered or stale request returns its error code |
| `task_comment` | write | `id`, `body` | The appended comment and its author |
| `task_intake` | write | `title`, `body`, `project`, optional `lane` | The triage task, `created`, and duplicate candidates found before filing |

Every task result carries `console_url`
(`https://console.robinco.dev/ui/tasks/<id>`) and, when the task has a YouTrack
mirror, the readable YouTrack id and URL. Limits are clamped, not rejected:
`task_search` stops at 50; `task_list`, `task_comments`, `task_events` and
`changes_since` stop at 100.

## Field ownership

`task_get` answers with `field_sources`, the canonical system for every task
data field in that response (hk:doc
`design/2026-11-07/youtrack-first-operator-ui`):

| Field(s) | Source |
| --- | --- |
| `id`, `title`, `body`, `body_doc`, `state`, `priority`, `kind`, `lane`, `project`, `area`, `labels`, `claimed_by`, `created_by`, `refs`, `created_at`, `updated_at`, `started_at`, `completed_at`, `canceled_at`, `closed_at`, `parent`, `children`, `relations`, `blockers`, `project_owner`, `comments_preview`, `events_preview`, `console_url` | `hk` (the hk Postgres work record) |
| `comments_by_agents` (agent comments) | `hk` |
| `operator_comments` | `youtrack` |
| `translations`, `korean_display_text`, `youtrack` (the mirror) | `youtrack` |

Response metadata (`comments_more`, `events_more`, `field_sources` itself) is
not a task field and has no entry. A reader that is about to change a value
must write to its owning system, not to whichever copy it happened to read.

## Paging and the change feed

Cursors are opaque strings. A client never builds or parses one; it sends back
the last `next_cursor` it received. An empty string starts at the beginning.

`task_list` pages in `(updated_at, id)` order, oldest first, so every page is
deterministic. Send `cursor` (an empty string for the first page) and follow
`next_cursor` until it is empty. `updated_since` (RFC3339) starts at tasks
updated at or after that time and can be combined with a cursor. Without
`cursor` or `updated_since`, `task_list` keeps its previous unpaged answer.
A task updated while a client is paging moves ahead of the cursor; use
`changes_since` when exactly-once matters.

`task_comments` and `task_events` page by id, oldest first, and never repeat or
skip a row between calls.

`changes_since` is the feed behind `handoffkeep since` and `GET
/v1/sync/delta`: the hk `sync_log`. Every task create/update/delete, comment,
decision and document write appends one row with a monotonic `sync_id` when its
transaction commits. A state transition is a `task_updated` row. Its
`task_current` summary is the task's state when the feed is read, not the state
at that historical row: a task with several rows in one page repeats the same
summary, and `task_events` is the ordered transition history. Each item carries
its own `cursor`; `next_cursor` is the position after the last row returned, so
processing a page and sending the cursor back visits every change exactly once.
An idle call returns no items and the same cursor. `max_cursor` is the feed's
current end, useful for a watcher that wants to start from now instead of
replaying the retained history.

The feed keeps `HANDOFFKEEP_SYNC_RETENTION` of history (30 days by default).
A cursor older than the pruned boundary is answered with `reset:true` and no
items. `min_cursor` is the position immediately before the oldest retained row
(`oldest_retained_sync_id` names that row), so resuming from it returns every
retained change; the gap before it is the pruned history. A client that must
not lose changes should poll more often than the retention window.

## Intake

`task_intake` files through the existing triage intake path. Source is
`chatgpt`; the source reference is a digest of the title, body and project, so
an identical repeat returns `created:false` and the task already recorded.
Candidate duplicates come from the same task search the console uses and are
returned with the new task.

## Turning it off

Unset the three `HANDOFFKEEP_MCP_*` variables and restart: the Access
credential is off and bearer clients keep working. Disabling the Access
application or removing the tunnel route makes `hk-mcp.robinco.dev`
unreachable; `/mcp` on the existing origin is unaffected. The change adds no
schema migration and writes no configuration.
