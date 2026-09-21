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
| `HANDOFFKEEP_UI_DIRECTOR_LANES` | Comma-separated lanes whose task decisions are pinned for approval | `lane-a` |
| `HANDOFFKEEP_UI_ADMIRAL_LANES` | Legacy alias — 병기 수용, 제거는 별도 태스크. Comma-separated lanes whose task decisions are pinned for approval | `lane-b` |

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

`/ui` accepts `GET` plus only `POST /ui/decisions/answer`,
`POST /ui/decisions/answer-batch`, and `POST /ui/compose`.
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
- `/ui/queue` mounts the React queue board in the existing queue slot. Cards
  group by canonical state and order deterministically by priority then id.
  Client-side filters (lane, kind, state toggles, title/id search, 운영자
  필요만) never drop server data; the board polls the same-origin BFF every
  fifteen seconds with non-overlapping refreshes and keeps the last good data
  on failure. Selecting a card fetches its detail.
- `GET /ui/api/board/tasks` is a `no-store` JSON list with `lane`, `state`,
  `parent_lane`, exclusive `after_id`, and `limit` (default 200, max 500)
  parameters in stable id order. `truncated` and `next_after_id` continue the
  page; the client owns grouping and ordering.
- `GET /ui/api/board/tasks/<id>` returns the task with refs, the append-only
  transition history, per-state dwell segments, the joined Linear identifier
  when present, and participant segments aggregated from `bench_reps` with an
  exact `task_ref=hk:task/<id>` match. Empty telemetry is `not_collected`;
  when matching reps exceed the 500-rep bound, `truncated` marks the segment
  totals as partial rather than complete.
- `GET /ui/api/policy/active` resolves the exact `policy/active` pointer to
  its manifest document key, validates the manifest as a single JSON document
  (a second value or trailing content is `invalid_manifest`), and lists
  manifest items with `/ui/doc/<key>` links and `exists` flags. Statuses are
  `not_configured`, `invalid_pointer`, `manifest_missing`,
  `invalid_manifest`, or `ok`; beyond 200 items `truncated` is set.
- `/ui/decisions` shows only unresolved items. The definitions are exact:
  1. A task is shown when `state='needs_decision'`; its question is the `note`
     from that task's latest `task_events` row with `to='needs_decision'`.
  2. A `job.escalate` relay event is *open* when no later-ID event with the
     same `job_id` has kind `job.joined` or `job.completed`, and no later
     `lane.event` from the same `owner_lane` has an `event_id` matching
     `%decision-escalation-<escalation id>-%`. This event-ID condition closes
     web and CLI escalation answers without allowing an unrelated task answer
     with the same numeric ID to close the escalation. An open escalation is
     *answerable* only when its effective question — `question`, else `text`,
     else `report_last_line` — begins with `[decision-needed]` after leading
     whitespace. Every other open escalation is an operational signal and is
     retained under the folded `signals` section with no answer controls and
     no contribution to the answerable-card budget; signal retention is a
     display classification, not resolution — a signal still closes only
     through the two open-condition mechanisms above. The marker is stripped
     for the answer form's question display only; durable event text is never
     rewritten.
  3. A `lane.event` whose `text` begins `[decision-needed]` is shown when no
     later-ID `lane.event` from the same `owner_lane` has text beginning with
     the exact prefix `[decision-answered] #<event id>:` naming that question's
     durable id.

  The P2 lane-answer route emits a later event beginning exactly
  `[decision-answered] #<event id>:`; that exact-prefix and same-owner-lane
  relationship closes only the question with that id. Other
  `[decision-answered]` messages — generic text, a different id, or another
  lane — do not close it.
- `/ui/fleet` is a React session table mounted in the existing fleet slot. The
  browser calls only `GET /ui/api/fleet` every ten seconds. `/ui/queue` is the
  other React page; timeline and decisions remain htmx. React pages and every
  `/ui/api/*` response send
  `Content-Security-Policy` (`default-src 'self'; connect-src 'self'; …`); htmx
  pages keep their inline script and do not receive that header.
- `GET /ui/api/fleet` is a `no-store` JSON projection of hub `/v1/nodes`
  `session_snapshot`. Each node carries `machine_id`, `state`, `last_ping`,
  sessions (`pane_id`, `workspace_id`, `label`, `status`, `interactive_ready`),
  `snapshot_status`, `truncated`, original hub `received_at`, and `stale`. The
  envelope has `fetched_at` (last **successful** hub read) and
  `upstream` (`ok|timeout|auth_failed|http_error|unconfigured`). Session `model`
  is always `미수집`. Empty `sessions` with `snapshot_status=ok` is not
  collection failure. Hub credentials and the hub URL never appear in the
  response. Shared polling means at most one upstream `/v1/nodes` read per ten
  seconds (single-flight + cache); the three-second hub timeout and last-success
  snapshot are retained. This route does not call jobs, relay, or lane.event.

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

The decisions page presents forms for unresolved tasks, open
`[decision-needed]` job escalations, and open `[decision-needed]` lane
events. A question line beginning `options:`
is split on `|` into up to eight radio choices with an `only=<n>` submit button;
otherwise the operator enters free text. Tasks in `HANDOFFKEEP_UI_DIRECTOR_LANES`
(with the legacy alias `HANDOFFKEEP_UI_ADMIRAL_LANES` accepted alongside it)
appear first under
**Awaiting your approval**, with their task references, and are not repeated in
the ordinary task section. Open job escalations without a leading
`[decision-needed]` marker are operational signals: they are retained under
the folded `signals` section rather than treated as questions, and direct
answer writes against them are rejected like unknown or closed decisions.

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
console paths. `tasks.decisions_pending` counts generic `needs_decision` tasks
plus open `[decision-needed]` lane events; open disposition items are reported
separately as `tasks.dispositions_open` and are not part of that sum.
`tasks.by_state.needs_decision` remains the raw state tally and includes both. Hub setup, transport, decoding, and non-200 failures retain a
200 response with empty hub arrays and only `unconfigured`, `unreachable`, or
`status_<code>` as the health error. The body is capped at 256 KiB by dropping
oldest entries from `tasks.active` and marking `truncated`.

`POST /ui/api/nodes/{machine}/accepting` accepts JSON `{ "accepting": bool,
"reason": string }`, validates a lowercase machine name and a 120-byte reason,
then relays it to the hub with the server-side credential. Hub status and up to
64 KiB of its body are forwarded without exposing hub configuration; an
unconfigured or unreachable hub returns `502 {"error":"hub_unavailable"}`.

## Disposition items (#493)

`/ui/decisions` renders a **처분 대기** section above the generic form. Its
header is `DispositionSummary` (the same function as
`handoffkeep tasks disposition summary`): `미처분 n · 최고령 x일`, then
`다음 묶음 m건` when more than 50 items are open, and a detail line with
pending application, holds, merged PRs without an item (candidates), and the
last 24 hours of batch and single answers. Disposition items never appear in
the generic task cards or `mode=recommended`; glance reports them as
`tasks.dispositions_open`, outside `tasks.decisions_pending`. The generic answer
routes refuse them before any hub emit. Origins and options are fixed at
creation (transition refs patches touching `origin_pr`, `origin_task`, or
`decision_options` are refused in every state), and re-asking
(`→ needs_decision`) clears the previous answer.

- `POST /ui/dispositions/answer` (`id`, `gen`, `key`) records the answer first
  (`needs_decision → claimed`, `by=operator:<email>`, `refs.disposition.answer`)
  and then emits `[decision] #<id>: <key>: <label> (from operator(web) <email>)`
  with event ID `web-disposition-<id>-g<gen>`. A changed question generation is
  409; a second answer is 409 and emits nothing.
- `POST /ui/dispositions/accept-batch` answers, in one transaction, exactly the
  oldest-50 snapshot the page rendered. The snapshot (`id:gen` list) is signed
  with the process key and bound to the operator email, batch ID and issue time
  (12 h). Items created after rendering are not in it; items whose generation
  changed are skipped. One lane event per lane:
  `[decision] disposition-batch <batch>: #a=A #b=C … (from operator(web) <email>)`
  with ID `web-disposition-batch-<batch>-<lane>`.
- If the emit fails the answer stays recorded and the item is listed under
  **통지 대기**; `POST /ui/dispositions/renotify` (`event_id`) re-sends the same
  text under the same event ID until that event reaches `relay_events`.

These routes are outside `/ui/api/`, so `ServeHTTP` refuses Access service
identities, and each handler independently requires an Access **email**
identity before origin, CSRF, hub, or store checks. Requests without an Access
assertion — including ones sent straight to the tailnet listener, which serves
the same mux — are 401 before any handler runs.

## P4 decision options, batch answers, and resolve

Structured choices are stored additively in `tasks.refs.decision_options`; no
schema migration is required. Each key is `A` through `F`, labels are trimmed
to 1--120 bytes, and at most one choice is recommended. The closed lane-event
syntax is a final line such as:

```text
[options] A|노드 로컬 저장;B|중앙 저장 선행;rec=A;free=0
```

Options use `;`, key and label use `|`, and `rec=<key>` is optional immediately
before optional final `free=0`. Omitting `free=0` permits direct answers. The
parser recognizes options only when the final line fully matches; malformed
tokens, duplicate keys, unknown recommendations, too many options, or an
options line elsewhere stay ordinary body text. The question and serialized
options together must fit the 2048-byte event limit.

The Decisions page renders structured choices as radios. A recommendation is
default-selected and marked `권고`; when permitted, a direct-answer radio and
input are available and a nonempty direct answer wins. Legacy `options:` lines
also use the batch radio plus `only=<n>` convention, accepting only their parsed
options and exposing no direct-answer field; plain questions retain the free-text path.

All cards share `POST /ui/decisions/answer-batch`. Cards provide
`items.<n>.type`, `items.<n>.id`, `items.<n>.select`,
`items.<n>.answer`, `items.<n>.custom`, and `items.<n>.note`. `only=<n>` takes
priority; otherwise `mode=recommended` sends every open structured item with a
recommendation, then `mode=selected` sends checked cards. The maximum is 50
items. Empty selection sends nothing and reports `선택된 항목이 없습니다.`

Each batch item independently follows normal validation, hub emission, then
task transition, in card order. After authentication and form checks the HTTP
response is 200; rows expose `data-item-status` values: 200 for
`전송됨(event_id=...)`, 409 for `이미 답변됨`, 502 for a lane-send failure,
and 400 or 500 for other errors. Processed items each emit exactly one
`action=decision-batch` audit entry without answer text, hub credentials, or
CSRF values.

Use `handoffkeep decisions resolve <task|escalation|lane> <id> --by <lane>
--answer <answer>` to close an answer made outside the console. It records a
`cli-decision-<type>-<id>-<8 hex>` lane event and transitions a task to
`claimed`. `--no-inject` marks that new event delivered as `resolve/<lane>`;
without it, normal node injection sees the event as undelivered.

The board's **운영자 필요만** preset mirrors the retired htmx operator view as a
client-side filter only: the backlog column collapses to a count and other
columns show nonterminal `decide` and `needs_decision` work. It narrows the
rendered cards, never the API response — `GET /ui/api/board/tasks` always
returns the complete task set for the requested lane.
