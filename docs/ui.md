# Fleet console (`/ui`)

The optional fleet console is an Access-protected view of relay events, the task
queue, decision inbox, session checkpoints, and the configured hub. It also has
two operator write routes for answering decisions and composing lane events.
`GET /ui` redirects to `GET /ui/timeline`.

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
| `HANDOFFKEEP_UI_LANES` | Comma-separated compose destination lanes | `lane-a,lane-b` |
| `HANDOFFKEEP_UI_ADMIRAL_LANES` | Comma-separated lanes whose task decisions are pinned for approval | `lane-a` |

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

`/ui` accepts `GET` plus only `POST /ui/decisions/answer` and `POST /ui/compose`.
All other non-GET UI methods receive `405 Allow: GET`. It does not read API
bearer tokens. Conversely, `/v1/*` and `/metrics` do not read Cloudflare Access
assertions. This prevents either credential type from substituting for the
other.

The absent-route 404 is fail-closed: an incomplete Access configuration cannot
accidentally expose a console without origin-side verification. Hub credentials
remain in the server process; browser responses, templates, static assets, and
SSE messages contain only the fetched operational fields, never the hub token.

Email identities may use every console route. An allowlisted service identity
may use only `/ui/api/*`; every HTML, fragment, document, static, SSE, and
operator-write route returns `403`. An empty
`HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES` allowlist creates no service identity
path, while the email allowlist behavior remains unchanged. Service POSTs have
no browser cookie or CSRF requirement, but must use `Content-Type:
application/json`; email API POSTs retain the same-host Origin/Referer and
email-bound `hk_ui_csrf` validation used by the operator forms.

All database content is passed to Go's `html/template` without typed HTML,
JavaScript, or URL wrappers. A task PR is linked only when its stored value
starts with `https://github.com/`; other values are rendered as text.

## Pages and data

- `/ui/timeline` shows relay events in descending durable-ID order. It supports
  `lane`, `kind`, `since`, `until`, and exclusive `before_id` parameters. Date
  bounds apply to `received_at` in UTC (`since` inclusive, `until` inclusive
  through the end of that UTC day). Pages contain at most 200 events; the store
  hard-caps any timeline query at 1000.
- `/ui/queue` is a lane-by-state task board. Expanding a card fetches its
  append-only task transition history.
- `/ui/decisions` shows only unresolved items. The definitions are exact:
  1. A task is shown when `state='needs_decision'`; its question is the `note`
     from that task's latest `task_events` row with `to='needs_decision'`.
  2. A `job.escalate` relay event is shown when no later-ID event with the same
     `job_id` has kind `job.joined` or `job.completed`.
  3. A `lane.event` whose `text` begins `[decision-needed]` is shown when no
     later-ID `lane.event` from the same `owner_lane` has text beginning
     `[decision-answered]`.

  The P2 lane-answer route emits a later event beginning exactly
  `[decision-answered]`; that prefix and same-owner-lane relationship close the
  item. Other messages do not close it.
- `/ui/fleet` fetches `/v1/nodes` and, when available, `/v1/jobs` through the
  server-side hub proxy. Both calls have a three-second bound. Missing hub
  configuration affects only this panel. A failed nodes fetch retains the last
  successful snapshot and marks it stale; without one the page says the hub is
  unavailable. A 404 or 405 from `/v1/jobs` means the jobs API is unsupported.

## Live refresh and vendored assets

`GET /ui/events` is an SSE stream with `Content-Type: text/event-stream` and
`Cache-Control: no-cache`. It sends an initial `delta` event then polls the
maximum `relay_events.id` and `task_events.id` every ten seconds. Changed IDs
produce:

```text
event: delta
data: {"relay_max_id":123,"task_event_max_id":456}
```

The page uses that event to have htmx fetch its current `/ui/fragments/...`
view. It uses vendored htmx 2.0.4 from `internal/ui/static/htmx.min.js`; the
adjacent `htmx.LICENSE` is the htmx 0BSD license. No external CDN is used.

## P2 operator write path

The decisions page presents forms for unresolved tasks, open job escalations,
and open `[decision-needed]` lane events. A question line beginning `options:`
is split on `|` into up to eight answer buttons; otherwise the operator enters
free text. Tasks in `HANDOFFKEEP_UI_ADMIRAL_LANES` appear first under
**Awaiting your approval**, with their task references, and are not repeated in
the ordinary task section. Job events that are operational signals are retained
under the folded `signals` section rather than treated as questions.

`GET /ui/compose` provides a destination dropdown from
`HANDOFFKEEP_UI_LANES`. If that setting is empty, or if either hub setting is
missing, write forms are disabled and direct POSTs are rejected. Compose wraps
the submitted text as `[event] <text> (from operator(web) <email>)`; the final
wrapped text, not merely the input, must be at most 2048 bytes and contain no
NUL, C0, or C1 controls. The page's byte counter and the server both enforce
the limit. Timeline's `✓ delivered` marker is the delivery-status view.

Both write routes invoke the server-side hub ingress before any task state
change:

```text
POST /v1/relay/events
Authorization: Bearer <hub token>
Content-Type: application/json

{"kind":"lane.event","lane":"lane-a","event_id":"web-msg-0123abcd","text":"…","label":"operator-web"}
```

The hub returns `201` for an accepted event and `409` with
`duplicate_event_id` for a duplicate; both are successful UI sends, while `409`
is displayed as “already sent”. A `400`, `401`, `502`, timeout, or network
failure is a lane-send failure. For a task answer only, a `201` or `409` is
followed by `TransitionTask(..., "claimed", "operator:<email>", ...)`.
Therefore a failed emit never transitions the task. A conflict is shown as
“already answered”; any other transition failure names the emitted event ID and
asks for manual transition. Lane answers use `[decision-answered]` as described
above. Normal form POSTs redirect with a fixed relative `303`; htmx POSTs
replace `#ui-content` immediately.

Every UI POST first verifies the same Cloudflare Access assertion as GET, then
requires a same-host `Origin` (or, only when Origin is absent, `Referer`) and a
CSRF token. The process creates a fresh random 32-byte HMAC key. Form tokens
are email-bound, expire after 12 hours, and are mirrored in the secure,
HttpOnly, `SameSite=Strict`, `/ui`-scoped `hk_ui_csrf` cookie. A failed check
has no hub or database write. Each POST produces one server audit line with
only `email`, `action`, `target`, `event_id`, and `result`; message text, hub
credentials, and CSRF values are never logged.

`GET /ui/doc/<key>` opens a stored document in an escaped `<pre>`. Keys reject
leading `/` and `..`. Event text tokens of the form `doc:<key>` (where key uses
`[A-Za-z0-9._\-/]`) link to that viewer without typed HTML. For events written
by hub HTTP ingress, a `reason` beginning `http_ingress:` renders the trailing
producer label as a sender badge; other `reason` values remain hidden.

## P3 glance API

`GET /ui/api/glance` is a `no-store` JSON snapshot for service clients. It
contains the UTC generation time, sanitized hub health, raw hub `nodes`,
`lanes`, and `jobs` data (with only per-node `active_jobs` added), seven task
state totals, unresolved decisions, the newest 20 active tasks, and fixed
console paths. Hub setup, transport, decoding, and non-200 failures retain a
200 response with empty hub arrays and only `unconfigured`, `unreachable`, or
`status_<code>` as the health error. The body is capped at 256 KiB by dropping
oldest entries from `tasks.active` and marking `truncated`.

`POST /ui/api/nodes/{machine}/accepting` accepts JSON `{ "accepting": bool,
"reason": string }`, validates a lowercase machine name and a 120-byte reason,
then relays it to the hub with the server-side credential. Hub status and up to
64 KiB of its body are forwarded without exposing hub configuration; an
unconfigured or unreachable hub returns `502 {"error":"hub_unavailable"}`.
