# Fleet console (`/ui`)

The optional fleet console is an Access-protected React app (`web/console`,
built into `internal/ui/static/console`, committed) plus the JSON routes it
calls under `/ui/api/*`. The server renders one shell page for every view
route; the browser picks the view from `location.pathname`. There are no
server-rendered pages, no htmx, and no SSE stream.

`GET /ui` serves the Home view. The console's views are:

| Path | View |
| --- | --- |
| `/ui`, `/ui/home` | Home: what is waiting on the operator (`/ui/api/attention`) |
| `/ui/queue` | Board mount point; the React Work page takes the same data |
| `/ui/work` | Work: task board and task detail (`/ui/api/board/*`) |
| `/ui/tasks/{id}` | Task record (deep link) |
| `/ui/decisions`, `/ui/decisions/{id}` | Decision list and one decision |
| `/ui/deploys` | Deploy status (`/ui/api/deploy-pending`) |
| `/ui/work-status` | Work status (`/ui/api/work-status`) |
| `/ui/doc/{key}` | Document viewer (`/ui/api/board/doc`) |

Retired paths (`/ui/timeline`, `/ui/compose`, `/ui/fleet`, `/ui/grades`,
`/ui/docs`, `/ui/events`, `/ui/decisions/legacy`) are not routed; the fleet
page is the panewire hub's `/ui`.

## Configuration

The console is mounted only when all three Cloudflare Access settings are
nonempty after trimming whitespace. If any is absent, `/ui` is not registered
and returns the normal 404 response.

| Environment variable | Purpose | Example |
| --- | --- | --- |
| `HANDOFFKEEP_UI_CF_TEAM_DOMAIN` | Cloudflare Access team domain | `example.cloudflareaccess.com` |
| `HANDOFFKEEP_UI_CF_AUD` | Access application audience tag | `example-ui-audience` |
| `HANDOFFKEEP_UI_ALLOWED_EMAILS` | Comma-separated UI email allowlist | `admin@example.com` |
| `HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES` | Comma-separated Access service-token `common_name` allowlist for `/ui/api/*` | `glance-reader` |
| `HANDOFFKEEP_HUB_URL` | Optional hub origin for fleet status | `http://127.0.0.1:9000` |
| `HANDOFFKEEP_HUB_TOKEN` | Optional server-side hub credential | set outside source control |

Create a Cloudflare Access application for the UI origin, set its team domain
and audience tag in the first two settings, and add an Access policy that
admits only the identities intended to operate the console. The origin repeats
that check: it verifies the assertion signature against the team JWKS, requires
RS256, issuer, audience, expiration, and an allowlisted email. The email check
trims whitespace and compares ASCII case-insensitively.

The JWKS endpoint is derived as
`https://<team-domain>/cdn-cgi/access/certs`. RSA keys are cached for ten
minutes. An unknown key ID triggers one immediate refresh at most once per
minute; no usable cached key or failed retrieval is an unauthenticated request.

## Security boundary

`/ui` does not read API bearer tokens, and `/v1/*` and `/metrics` do not read
Cloudflare Access assertions. This prevents either credential type from
substituting for the other.

The absent-route 404 is fail-closed: an incomplete Access configuration cannot
accidentally expose a console without origin-side verification. Hub credentials
remain in the server process; browser responses, the shell page, and static
assets contain only fetched operational fields, never the hub token.

Email identities may use every console route. An allowlisted service identity
may use only `/ui/api/*`; every page, document, file, static, and
operator-write route returns `403`. An empty
`HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES` allowlist creates no service identity
path. Service POSTs have no browser cookie or CSRF requirement, but must use
`Content-Type: application/json`.

Every UI POST first verifies the same Cloudflare Access assertion as GET, then
requires a same-host `Origin` (or, only when Origin is absent, `Referer`) and a
CSRF token. The process creates a fresh random 32-byte HMAC key. Form tokens
are email-bound, expire after 12 hours, and are mirrored in the secure,
HttpOnly, `SameSite=Strict`, `/ui`-scoped `hk_ui_csrf` cookie. A failed check
has no hub or database write. Each POST produces one server audit line with
only `email`, `action`, `target`, `event_id`, and `result`; message text, hub
credentials, and CSRF values are never logged.

Any other non-GET UI method receives `405 Allow: GET`.

All database content is rendered as text. A task PR is linked only when its
stored value starts with `https://github.com/`.

## Operator writes

| Route | Writes |
| --- | --- |
| `POST /ui/api/decision-requests/resolve` | Answers a structured `dr-` decision request |
| `POST /ui/api/decision-requests/deny` | Denies one `pending_confirmation` attempt |
| `POST /ui/api/tasks/{id}/priority` | Task priority |
| `POST /ui/api/tasks/{id}/{action}` | Task lifecycle actions (close, hold, reopen, relane, drop) |
| `POST /ui/api/inbox/state`, `POST /ui/api/inbox/undo` | Attention inbox state |
| `POST /ui/tasks/{id}/comments` | Task comment |
| `POST /ui/api/nodes/{machine}/accepting` | Fleet host accepting flag (relayed to the hub) |

Each write verifies the signed-in email, origin, and CSRF token before any hub
or store call. Decision answers return a structured result (`answered`,
`denied`, `pending_confirmation`). An Access identity is not an O3 central
principal; where the write needs one, the store's fence refuses it.

### Decision requests

Two JSON routes answer a structured `dr-` request through
`Store.ResolveDecision` on the console channel. Both require an Access email
identity (a service identity is `403 operator_only`), a same-origin request
(`403 origin_rejected`) and the header CSRF token (`403 csrf_rejected`); the
body is at most 4 KiB, unknown fields and trailing data are `400
invalid_request`, and `event_key` is required (`400 event_key_required`). The
same `event_key` with the same body replays the first result.

- `POST /ui/api/decision-requests/resolve` with `{request_id, option, text,
  event_key}` answers the request as `operator_direct`.
- `POST /ui/api/decision-requests/deny` with `{request_id, attempt_id, receipt,
  event_key}` denies one `pending_confirmation` attempt of the current request
  revision: a new attempt with result `denied`, the operator's optional
  `receipt` and a `decision-denied` task event; the request stays open. A target
  that is not pending, or belongs to another task or revision, is `409
  decision_attempt_not_pending`; an attempt already denied is `409
  decision_attempt_denied`; an unknown attempt is `404 not_found`.

Results and statuses follow `api.DecisionResolveResponse`: `answered` and
`denied` are 200, `pending_confirmation` 202, and a request that is no longer
open is `409 decision_request_resolved`.

## Read APIs

All `GET` responses are `no-store` JSON. Each route is an email-only or
email-or-service route as noted.

- `GET /ui/api/board/tasks` lists tasks with `lane`, `state`, `parent_lane`,
  exclusive `after_id`, and `limit` (default 200, max 500) parameters in stable
  id order. `truncated` and `next_after_id` continue the page.
- `GET /ui/api/board/tasks/<id>` returns the task with refs, the append-only
  transition history, per-state dwell segments, the joined Linear identifier
  when present, and participant segments aggregated from `bench_reps` with an
  exact `task_ref=hk:task/<id>` match. Empty telemetry is `not_collected`;
  when matching reps exceed the 500-rep bound, `truncated` marks the segment
  totals as partial rather than complete.
- `GET /ui/api/board/tasks/<id>/comments`, `.../attachments`,
  `GET /ui/api/board/projects`, `GET /ui/api/board/lanes`,
  `GET /ui/api/board/doc?key=` and `GET /ui/api/board/doc/attachments?key=`
  serve the board and document views. Document routes are email-only.
- `GET /ui/api/decisions` and `GET /ui/api/decisions/<id>` list and read
  structured decision requests. `GET /ui/api/attention` lists what waits on the
  operator (Home).
- `GET /ui/api/work-status` and `GET /ui/api/deploy-pending` serve the Work
  status and Deploys views. Deploy status is assembled from deploy-record
  documents and task events; it is email-only.
- `GET /ui/api/fleet` is a projection of hub `/v1/nodes` `session_snapshot`.
  Each node carries `machine_id`, `state`, `last_ping`, sessions (`pane_id`,
  `workspace_id`, `label`, `status`, `interactive_ready`), `snapshot_status`,
  `truncated`, original hub `received_at`, and `stale`. The envelope has
  `fetched_at` (last **successful** hub read) and `upstream`
  (`ok|timeout|auth_failed|http_error|unconfigured`). Session `model` is always
  `미수집`. Empty `sessions` with `snapshot_status=ok` is not collection
  failure. Hub credentials and the hub URL never appear in the response. Shared
  polling means at most one upstream `/v1/nodes` read per ten seconds
  (single-flight + cache); the three-second hub timeout and last-success
  snapshot are retained. This route does not call jobs, relay, or lane.event.
- `GET /ui/api/glance` is a `no-store` JSON snapshot for service clients. It
  contains the UTC generation time, sanitized hub health, raw hub `nodes`,
  `lanes`, and `jobs` data (with only per-node `active_jobs` added), task state
  totals (the twelve states plus `awaiting_decision`), unresolved decisions, the
  newest 20 active tasks, and `console_paths` (`decisions`, `queue`).
  `tasks.decisions_pending` counts generic tasks with `awaiting_decision` set
  plus open `[decision-needed]` lane events. Hub setup, transport, decoding,
  and non-200 failures retain a 200 response with empty hub arrays and only
  `unconfigured`, `unreachable`, or `status_<code>` as the health error. The
  body is capped at 256 KiB by dropping oldest entries from `tasks.active` and
  marking `truncated`.

`POST /ui/api/nodes/{machine}/accepting` accepts JSON `{ "accepting": bool,
"reason": string }`, validates a lowercase machine name and a 120-byte reason,
then relays it to the hub with the server-side credential. Hub status and up to
64 KiB of its body are forwarded without exposing hub configuration; an
unconfigured or unreachable hub returns `502 {"error":"hub_unavailable"}`.

## Frontend rules

React views poll through `usePoll`, which skips ticks while the tab is hidden
and reads once as soon as it becomes visible again. Every `/ui/api/*` response
and every shell page sends `Content-Security-Policy` (`default-src 'self';
connect-src 'self'; …`). Assets are served from `/ui/static/console/` with a
build stamp (`?v=<vcs revision>`); the stamped URL is cached for a day, every
other request revalidates. No external CDN is used.

The bundle is rebuilt with `npm run build` in `web/console` and committed
together with the source change; CI fails on a stale bundle.

## Disposition items

Disposition items (`tasks disposition`, `/v1/tasks/dispositions*`) are stored
and served by the bearer API and the CLI. The console no longer has a
disposition page or answer forms; the generic decision routes refuse these
items.
