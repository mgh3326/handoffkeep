# MCP for ChatGPT (`/mcp`)

hk serves the MCP streamable HTTP transport at `/mcp`. ChatGPT connects to
`https://hk-mcp.robinco.dev/mcp` through Cloudflare Access with Managed OAuth,
so the operator signs in once and the origin receives a signed Access user
assertion. The existing bearer-token clients keep using the same route.

## Dots requests

Create a standalone hk request with `handoffkeep dots request --title TITLE
--body BODY`, optionally adding `--requester-lane L` and `--project P`.
The authenticated caller is the default requester lane. The equivalent API
is `POST /v1/dots/requests` with `title`, `body`, `requester_lane`, and
`project`. Pass `--idempotency-key` to retry a request without creating a
second task. The API accepts the same value as `idempotency_key`. Each request
is a task in the `dots` lane; its body is an hk
document and its `parent_lane` identifies the requester.

A design request is filed with `--type CREATE_MOCKUP`, `CREATE_ASSET` or
`REVIEW_IMPLEMENTATION`. Its body must carry the policy template sections
`[목적]`, `[유지할 것]`, `[범위]`, `[제약]`, `[산출물]`, `[완료 기준]` and `[입력]`.
`[입력]` must name a commit SHA and the capture environment. `CREATE_ASSET`
may instead name the source material the asset is made from — an hk
attachment sha256 (64 hex characters), an `hk:doc` document key, or a
`원본:`/`source:` line that describes the material — and then needs neither.
Otherwise the request is refused. A refused request answers 400
`invalid_context` with a `reason` naming the rule that failed, and
`handoffkeep dots request` prints it after the code. The stored body starts
with a `[유형]` line. Requests with no type (research, for example) take no
template. `dots adopt` may record the adopted Canva design with
`--canva-url` (an https canva.com design link),
`--canva-page` and `--canva-version`, for `accepted` or `partially_used` results only. A repeat of such an adoption without the Canva flags keeps the recorded design.

A request may declare that it ends in the operator's choice. Pass
`--option 'A|label'` once per option (one to six, keys `A`–`F`) and
`--option-attachment 'A|sha256[|caption]'` for the pictures and videos that
show an option (at most eight per option). The API takes the same values as
`options: [{"key": "A", "label": "…", "attachments": [{"sha256": "…",
"caption": "…"}]}]`. The declaration is validated when the request is filed —
every address must name an attachment hk already holds — so a RESULT is never
refused because of it. A request with no declaration behaves as before.

On a declaring request, RESULT records the result as usual and opens the
operator's decision on the task in the same transaction: the declared options
with their materials, the RESULT document as the decision's `doc`, and dots's
own account — `recommended` (a declared option key), `recommendation_reason`,
`unverified` and `one_fix`, sent as `dots_reply` fields. Those four are allowed
only on RESULT, and `recommended` must name a declared key; when it is omitted
the decision opens with no recommended option. The task then waits on the
decision instead of completing. The operator answers on the console decision
page or in the dots chat (`decision_answer`); either way it is the same
decision record, the task completes with the decision and the answer as its
evidence, and the requester lane — and each `[알릴 레인]` lane — hears the answer
once as the request's result. Delivery is keyed by the decision, so a replayed
answer sends nothing. A task that is already settled keeps its closure (the
answer is recorded as cleanup), and an answer that lands while another decision
still keeps the task open completes it when that decision closes. `dots adopt`
still records what became of a result; answering the decision does not run it.

On the authenticated ChatGPT connection, `dots_inbox` lists open requests in
creation order. Use the returned `next_cursor` to fetch another page.
`dots_reply` takes `task_id`, structured `kind`, `body`, and an
`idempotency_key`. Reuse the key only when retrying the same reply. For `START`,
pass the task's `updated_at` from the inbox as `expected_updated_at`. START
claims the task for dots only if that revision is still current and unclaimed.
It returns a `run_token`. Every later reply on that run (`PROGRESS`, `BLOCKED`,
`QUESTION`, `RESULT`) must send the same `run_token`; the token is returned by
START only and is never listed by `dots_inbox`. A reply with no token is refused
with `dots_run_token_required`; a token that is malformed or is not the
request's active one is refused with `dots_run_token_mismatch`. A refused reply
writes nothing. A second START on a started request is refused. To take over a
started request, send START with `takeover: true` and the current
`expected_updated_at`: it returns a new `run_token`, the previous one is refused
from then on, and the handover is recorded in the task's events. `dots_inbox`
reports `started` and `run_started_at` (when the current run's token was
issued) for each request. Each request also lists its `attachments`
(`original_name`, `size_bytes`, `mime`, `sha256`, `created_at`) and, in
`referenced_attachments`, the attachments of up to five tasks its body names as
`hk <id>`, each with `from_task`. Both lists stop at 20 items and set
`attachments_truncated` or `referenced_attachments_truncated` when more exist;
`attachment_get_url` issues the ten-minute download link for one.
`PROGRESS` records a partial result and leaves the task open. `BLOCKED` parks
it with the reason, and `RESULT`
completes it with the result document as evidence — or, on a declaring request,
opens the operator's decision and leaves the task waiting on it (above). A
`QUESTION` is delivered
to the requester; set `ask_operator` when it needs an hk decision request and
the task should await that decision. A plain `QUESTION` is answered by the
request's requester lane with `handoffkeep dots answer TASK --text ...`
(`--requester-lane` defaults to `PANEWIRE_LANE`), or by
`POST /v1/dots/answer` with `requester_lane`. Answering closes the question
once: a second answer is refused with `no_open_dots_question`, and another
lane is refused with `task_conflict`. With more than one question open the
answer must name its target with `question_id` (`--question-id`, the reply's
`id` in `dots_inbox`); without one it is refused with
`dots_question_ambiguous`. `dots_inbox` shows the answer on the question it
closed. RESULT, BLOCKED, and QUESTION also create
a result document, result record, and lane notification. START creates only a
comment and the claim. PROGRESS creates a document, comment, and notification,
but no `dots_results` row, which records RESULT, BLOCKED and QUESTION only. Replies are
attributed to the authenticated operator.

The result's `source` is `hk`, or `linear` for a row the retired Linear
webhook recorded before hk 4425.

The request's requester lane cancels its own request with `handoffkeep dots
cancel TASK --reason REASON`, or `POST /v1/dots/requests/{id}/cancel` with
`requester_lane` and `reason`. A comment does not cancel a request: the task
settles as `dropped` (category `canceled`), the reason is recorded as its
closure evidence, the request leaves `dots_inbox`, and later `dots_reply`
calls on it are refused as on any settled task. `requester_lane` is
self-asserted, exactly as on create: it must name the request's `parent_lane`,
and the authenticated client is recorded as the transition's actor. Another
lane answers 409 `task_conflict` with a `reason` naming the rule. A retried
cancel of an already dropped request answers 200 with the request, and the
cancel supersedes the request's run token. A cancel without a reason, or on a
task that is not an hk dots request, answers 400 `invalid_context` with a
`reason`.

`HANDOFFKEEP_DOTS_REPLY_WRITES` controls replies. It defaults to `off`, which
leaves `dots_inbox` available and makes `dots_reply` return
`dots_reply_writes_disabled`. Set it to `on` to enable replies, or to `off`
to stop them again.

## Documents cited by a task

A document has one binding: the task that owns it (`documents.task_id`). A task
can also cite a document it does not own by reference, so several requests can
check one living document, such as a schema document, without rebinding it
(hk 6125). A reference is a row in `task_doc_refs`; the binding is unchanged.

- `handoffkeep dots request ... --doc KEY` (repeatable, at most 20) records a
  reference on the request task in the same transaction as the task. The
  request's API field is `docs`. A key with no stored document refuses the whole
  request with 400 `doc_not_found`; no task is created.
- `handoffkeep tasks doc-ref add TASK --doc KEY` records references on an
  existing task. `tasks doc-ref rm TASK --doc KEY` drops one, and
  `tasks doc-ref list TASK` reads them.
- The API is `GET /v1/tasks/{id}/doc-refs`, `POST /v1/tasks/{id}/doc-refs` with
  `{"doc": KEY}` (201 when new, 200 when it already existed), and
  `DELETE /v1/tasks/{id}/doc-refs?doc=KEY`. An unknown task is 404 `not_found`;
  an unknown document is 400 `doc_not_found`.
- A reference whose document is later removed is not listed.
- The task page in the console lists bound documents, then referenced ones
  marked 참조. Each one opens the document view.

There is no MCP tool for references yet: the MCP surface has `dots_inbox` and
`dots_reply` only, and no request-creating tool to add `docs` to.

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

- `task_comment` stores the author `dots` for a write through this channel. The
  principal `operator:<email>` and channel `chatgpt` are recorded in the
  `mcp_write_audit` row and in the response, so the record reads as dots while
  the operator's own console writes keep `operator:<email>`.
- `decision_answer` closes the request with actor `operator:<email>` and, on an
  ordinary request, provenance `operator_direct`. A request marked operator_only
  (`answer_policy` `operator_direct`, `human_only`, or a disposition item) closes
  through dots only when the answer names the operator: `provenance`
  `operator_decided`, `operator_quote` with the operator's verbatim words, and
  `decision_ref` with the exact `request_id`. The resolution then keeps channel
  `dots` and provenance `operator_decided` (a relayed answer, not the verified
  path); without the words, or with a ref that binds another request or
  revision, the answer is refused (`operator_only_decision` or
  `provenance_ref_mismatch`) and the refusal is kept as an attempt. A
  disposition item is still answered only on the console.
- `task_intake` records `source=chatgpt` and `dots` as `created_by`; the tool
  response reports the exact `operator:<email>` principal and the `chatgpt`
  channel.

Decision answers record the actual MCP route: `mcp` for bearer callers and
`dots` for the verified ChatGPT Access channel. A human-only request is refused
to a bearer caller and closes on a verified operator path or on a dots answer
that carries the operator's words and the exact request id.

## Tools

Read tools set `readOnlyHint: true`. Write tools set `readOnlyHint: false` and
`destructiveHint: false`. `task_update` can change a task's state, lane,
priority and labels; the other write tools add decision or comment records,
file tasks, or attach files.

| Tool | Kind | Arguments | Result |
| --- | --- | --- | --- |
| `task_search` | read | `query`, optional `state`/`lane`/`project`, `limit` ≤ 50 | Task summaries from the console search path, including Korean translations |
| `task_show` | read | `id` | Title (with `title_ko` when present), state, priority, lane, project, `refs.pr`, last 20 comments, last 10 events |
| `task_get` | read | `id` | One task in full: body, state, priority, kind, lane, project, area, `claimed_by`, `refs`, parent, children, relations and blockers, `project_owner`, translations, YouTrack link, timestamps, `console_url`, `attachments`, `external_links`, `field_sync`, `field_sources`; comments and events are a five-item preview |
| `task_list` | read | optional `state`/`lane`/`project`, `limit` ≤ 100, `cursor`, `updated_since` | Task summaries; paged calls also carry `next_cursor` |
| `task_comments` | read | `id`, optional `cursor`, `limit` ≤ 100 | One page of comments, oldest first, with `next_cursor` and `has_more` |
| `task_events` | read | `id`, optional `cursor`, `limit` ≤ 100 | One page of events, oldest first, with `next_cursor` and `has_more` |
| `task_context` | read | `id` | One-call context bundle: `task` (the full `task_get` payload), `open_decisions`, `answered_decisions`, `decision_attempts`, `documents` bound to the task, `changes` (the task's mergewake change/PR rows), `field_sources`; each `external_links` entry carries `access` (`stored`) |
| `changes_since` | read | optional `cursor` or `since`, `limit` ≤ 100 | One page of the change feed: `items`, `next_cursor`, `max_cursor`, `has_more` |
| `decisions_pending` | read | none | Open structured requests with question, options (each with its `attachments`: `sha256`, `caption`, `mime`, `original_name`), `recommendation_reason`, `unverified`, `one_fix`, `question_ko`/`options_ko` when fresh (`options_ko` carries each option's key and translated label only; media and the recommendation are read from `options`), and state |
| `decisions_history` | read | optional `cursor`, `limit` | Closed structured requests plus session decisions with reading, verbatim quote, decision time and ground links; each closed request carries the same option attachments, `recommendation_reason`, `unverified` and `one_fix`. One `next_cursor` pages both lists. A cursor issued before session decisions were paged carries only the task position, so the session list restarts at its newest record once (bounded by `limit`, self-corrects on the next cursor) |
| `operator_attention` | read | none | Counts and bounded items for open decisions and tasks awaiting a decision or stalled |
| `decision_answer` | write | `request_id`, `option` or `text`, optional `note`, optional `idempotency_key`, and on the dots channel optional `provenance`/`operator_quote`/`decision_ref` for an operator-only request | Store result with channel `mcp` or `dots`; an operator-only request refuses a dots answer that lacks the operator's words and request id, keeping the refusal as an attempt; an already answered or stale request returns its error code |
| `task_comment` | write | `id`, `body`, optional `idempotency_key` | The appended comment, its author, and `replayed` (a repeated key returns the comment it first made) |
| `task_intake` | write | `title`, `body`, `project`, optional `lane`, optional `idempotency_key` | The triage task, `created`, and duplicate candidates found before filing; a repeated key returns the task that key first filed |
| `task_update` | write | `id`, `expected_updated_at`, `idempotency_key`, one or more of `state`, `lane`, `priority`, optional `note` | `changed`, the new `revision`, `replayed`, `console_url` and `youtrack_sync`; a stale revision returns `revision_conflict` and writes nothing |
| `decision_ask` | write | `task_id`, `idempotency_key`, `question`, `options`, `default_action`, optional `answer_policy`, `human_only`, `block` and the other `tasks decision-request` fields | The recorded request id and whether it was a duplicate |
| `decision_record` | write | `key`, `lane`, `reading`, `quote`, `decided_at`, `grounds` | Verified Cloudflare Access operators only; bearer callers receive `operator_required`. The CLI uses the reserved `operator` credential. Same-key retries replay, changed content is refused |
| `task_order` | write | `task_id`, `words`, `idempotency_key` | The recorded order and whether it was a duplicate. Only an operator principal: the `operator` bearer, or an Access operator relaying the operator's words through dots (recorded unverified). Any other caller receives `operator_required`; a task past backlog, unstarted or triage is `order_task_started`; the same key with other words is `order_key_conflict`. An order approves nothing and starts no worker |
| `task_order_ack` | write | `order_id`, `comment` | The acknowledged order; records `acked_at`, the acker and a task comment and clears the lane wait. A repeat returns the first acknowledgement; a withdrawn order is `order_not_open` |
| `attachment_list` | read | optional `ref`, `limit` | Immutable attachment metadata |
| `attachment_get_url` | read | `sha256` | Private presigned URL valid for ten minutes; an archived attachment answers `archived: true` with its `location` and no URL |
| `attachment_read` | read | `sha256` | The file inline. `image/png`, `image/jpeg`, `image/gif` and `image/webp` come back as image content; one above 1.5 MiB encoded is downscaled to a 1600 px long edge and re-encoded (PNG, or JPEG q85 for a JPEG), a header declaring more than 40 megapixels is refused without decoding, and one that still does not fit is refused. `text/*`, `application/json`, and a generic type named `.md`/`.tsv`/`.csv`/`.txt` come back as text up to 256 KiB, cut at a UTF-8 boundary with a line naming the full size; bytes that are not UTF-8 are refused. Every other type, and every refusal, returns a text block naming `attachment_get_url`. An archived attachment answers `archived: true` with its `location` instead of content |
| `attachment_put` | write | `content_base64`, `name`, optional `mime`, `ref` | Upload one private attachment without a server path |
| `attachment_upload_begin` | write | `name`, `mime`, `size`, `sha256`, optional `ref` | Start a direct upload and return an upload id and presigned PUT URL |
| `attachment_upload_complete` | write | `upload_id` | Finish a direct upload and return attachment metadata |
| `attachment_put_from_path` | stdio write only | `path`, optional `name`, `mime`, optional `ref` | Upload a local file; this tool is not exposed over HTTP |
| `projects_list` | read | none | Every project with `design_lane`/`effective_owner`, status and dates, open task counts by state, and `health` (in_progress, waiting, blocked, done, overdue counts; assignees; top blocked and overdue tasks) with `as_of` |
| `task_states` | read | none | The twelve states with category, terminal and claimable flags, allowed manual transitions and closure requirements |

The parity matrix — every console write route, `/v1` route, CLI command and MCP
tool, with the MCP gaps grouped as follow-ups — is [parity.md](parity.md). The
console write routes in that matrix are enforced by a test in
`internal/parity`.

An `idempotency_key` is at most 128 bytes and contains no NUL byte. A write
refuses any other key with `invalid_mcp_write` before it writes anything.

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
| `id`, `title`, `body`, `body_doc`, `state`, `priority`, `kind`, `lane`, `project`, `area`, `claimed_by`, `created_by`, `refs`, `created_at`, `updated_at`, `started_at`, `completed_at`, `canceled_at`, `closed_at`, `parent`, `children`, `relations`, `blockers`, `project_owner`, `comments_preview`, `events_preview`, `console_url`, `attachments`, `external_links` | `hk` (the hk Postgres work record) |
| `comments_by_agents` (agent comments) | `hk` |
| `operator_comments` | `youtrack` |
| `translations`, `korean_display_text`, `youtrack` (the mirror) | `youtrack` |

`field_sync` is the sync-time companion to the YouTrack rows: each of
`operator_comments`, `korean_display_text`, `translations` and `youtrack` has
`source` (`youtrack`), `status` (`synced` or `unknown`), `last_synced_at` (the
mirror's watermark, null when unknown) and `source_url` (the YouTrack issue
link). The watermark belongs to the whole mirror link, so the four fields share
it. A task with no mirror, or a link with no watermark yet, is `unknown`, never
omitted.

`attachments` lists the attachments bound to the task's body document (up to 50;
`attachments_more` says whether there are more). `external_links` lists the
task's pull requests (`pr`, `origin_pr`, `revert_pr`), `report`, `document`
(the body doc key) and `youtrack`; `url` is set only for an https
value.

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
an identical repeat returns `created:false` and the task already recorded. With
an `idempotency_key` the source reference is the key (scoped to the caller), so a
repeat returns the task that key filed even when its title or body changed.
Candidate duplicates come from the same task search the console uses and are
returned with the new task.

The three read-only meta tools answer questions about the queue itself.
`projects_list` reads the owner rule from the store, so `effective_owner` is
`design_lane` when set and `operator-desk` otherwise; its open counts exclude
the settled states. `health` buckets overlap: a blocked task that is also in
progress counts in both. `waiting` is triage, backlog and unstarted; `done` is
merged, deployed and completed; dropped and duplicate work is in neither. A
task is blocked while a `blocks` relation from an open task points at it, and
overdue while its due date is before `as_of`. Assignees are the claimers of
open tasks (up to 10); the top blocked and overdue lists hold 5 tasks per
project, highest priority and earliest due first. `task_states` derives the state graph, categories, terminal
flags and closure requirements from the tables the store enforces, never from a
second copy in this package.


## Turning it off

Unset the three `HANDOFFKEEP_MCP_*` variables and restart: the Access
credential is off and bearer clients keep working. Disabling the Access
application or removing the tunnel route makes `hk-mcp.robinco.dev`
unreachable; `/mcp` on the existing origin is unaffected. The change adds no
schema migration and writes no configuration.
