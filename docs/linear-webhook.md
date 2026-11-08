# Linear webhook for dots answers

The same endpoint also records operator decisions, on issues that carry a
decision request. See [linear-decisions.md](linear-decisions.md).

`POST /hooks/linear` receives Linear `Comment` and `Issue` webhooks. dots answers on issues
labelled `dots` with comments that start `[dots] <KIND>` (the kind word ends at a colon, so
`[dots] RESULT: <verdict>` is a RESULT whose verdict is the notification
summary), and this endpoint
records those answers as they arrive. A poller stays on as a reconciler; both
paths write the same document key, body and event id, so whichever runs second
changes nothing.

The separate [hk dots flow](mcp.md#dots-requests) creates independent hk
tasks and coexists with this webhook. No channel setting is required; Linear
comments continue to use this path.

Linear's webhook contract: <https://linear.app/developers/webhooks>.

## Configuration

Set both variables on the server, or the route stays unregistered and answers
404. Setting only one logs `linear_webhook.disabled reason=incomplete_config`.

| Variable | Value |
|---|---|
| `HANDOFFKEEP_LINEAR_WEBHOOK_SECRET` | The signing secret Linear shows for the webhook |
| `HANDOFFKEEP_LINEAR_API_KEY` | A Linear API key that can read the issues |
| `HANDOFFKEEP_LINEAR_CLIENT_ID`, `HANDOFFKEEP_LINEAR_CLIENT_SECRET` | Optional. A Linear OAuth application with **Client credentials** enabled. When both are set they replace the API key: hk mints an `actor=app` token (30 days) and renews it a day before expiry, so its comments and issues show as the application and can never count as the operator's answer. An app token reaches only the workspace's public teams unless the app is granted access to a private team on its details page. Setting only one of the two variables logs `linear_oauth.disabled reason=incomplete_config` and keeps the API key. |

One credential is needed (the API key, or the OAuth application, which wins
when both are set) because Linear's `Comment` payload documents no labels or
issue state, so the handler reads them from Linear's API. It skips that call
when `data.issue` has labels without `dots`, or has `dots` along with the
identifier, title, URL, state and any close time.

## What a delivery does

Every request is authenticated first:

- `Linear-Signature` must be the hex HMAC-SHA256 of the raw body. A missing or
  wrong signature gets `401 unauthorized`.
- `webhookTimestamp` must be within 60 seconds of the server clock. Otherwise
  the request gets `401 stale_timestamp`.

An authenticated `Comment` `create` event on an issue with no decision request is
then handled by its first line:

| Comment | Issue | Result | `status` |
|---|---|---|---|
| `[dots] RESULT`, `BLOCKED` or `QUESTION` | labelled `dots` | Document and notification | `recorded` |
| `[dots] START` or any other `[dots]` word | any | Nothing | `ignored` |
| Not starting `[dots]` | labelled `dots`, closed, comment newer than the close | Flag notification | `flagged` |
| Anything else | any | Nothing | `ignored` |

A repeated delivery answers `duplicate`, keeps the document and queues no
notification; it only fills a missing task link (see Document). If a Linear API
read fails, the endpoint answers `503 linear_unavailable` so that Linear
retries. Linear's `Comment` payload usually omits the issue description, so a
recorded or flagged comment costs one issue read for it.

### Document

The key is `dots/results/<ISSUE>/<YYYY-MM-DD>-<first 8 of comment id>`, with the
date taken from the comment's `createdAt`. The document has kind `note` and
session `operator-desk`. The first writer of a key wins.

The same transaction writes one `dots_results` row per comment: the document
key, the issue UUID and identifier, the comment id and kind, `verdict` (the
verdict on a `[dots] KIND: <verdict>` heading, else the summary line, at most
220 characters), `artefact_sha256` (the SHA-256 of the comment body as Linear
delivered it; the document's own `sha256` hashes hk's document body),
`routed_lane`, `task_id` and the adoption fields below.

**Task link.** When exactly one hk task has the origin `(linear, <issue UUID>)`
(`tasks origin add`), the row's `task_id` and the document's `task_id` name
it. With no such task, or several, both stay NULL: the link is unknown. When
the poller wrote the document first, the webhook still writes the row and sets
a NULL document `task_id`, and queues nothing.

### Notification

Each notification is one `notification_outbox` row, written in the same
transaction as its document. Its lane is, in order:

1. the lane on the first line of the issue description that starts
   `hk-lane:`, if it reads `hk-lane: <lane>` with a valid lane name and
   nothing else (an invalid first line falls through; later lines are ignored);
2. else the lane of the one task linked to the issue (see Task link);
3. else `operator-desk`.

The same order routes `RESULT`, `BLOCKED`, `QUESTION` and the closed-issue
flag. `BLOCKED` and `QUESTION` arrive on that lane as `decision` events: the
lane is the request's thread. hk opens no decision request for them; the
requester runs `handoffkeep tasks decision-request` when the operator must
answer. Read unsent rows with
`GET /v1/assistant/outbox`, and record a hub receipt with
`POST /v1/assistant/outbox/sent`. The server can do both itself: see
[Desk notification](linear-decisions.md#desk-notification).

| `kind` | Lane-event type | `event_id` |
|---|---|---|
| `dots_report` | `report` | `dots-<comment id>` |
| `dots_decision` | `decision` | `dots-<comment id>` |
| `dots_followup` | `info` | `dots-followup-<comment id>` |

The poller uses the same event ids. It still notifies `operator-desk` on its
own, whatever the routed lane. The hub dedupes on `(lane, event_id)`, so the two
paths fold into one event only when the routed lane is `operator-desk`;
otherwise the requester and the desk each get one. The handler adds no row when
the poller already wrote the document, because the poller has already notified.

### Adoption

A result starts at `adoption_state` `pending_review`. Record what became of it:

```bash
handoffkeep dots results --issue ROB-1        # or --task N
handoffkeep dots adopt 12 --state accepted --ref https://github.com/o/r/pull/3
handoffkeep dots adopt 12 --state superseded --superseded-by 15
```

The states are `accepted` and `partially_used` (both need `--ref`: a GitHub PR
URL, `hk:task/<id>` or `hk:doc <key>`), `rejected`, and `superseded` (needs
`--superseded-by`, another result id). `superseded` is final: any other change
answers `409 dots_adoption_final`. Repeating the stored change answers 200 with
`changed: false`. The routes are `GET /v1/dots/results?task=N|issue=<ID>` and
`POST /v1/dots/results/{id}/adoption` with `{"state", "ref", "superseded_by"}`.

## Register the webhook

Linear shows the signing secret only after the webhook exists, so the route goes
live at step 5.

1. Deploy the server with `HANDOFFKEEP_LINEAR_API_KEY` set.
2. If the hk origin is behind an access gate, allow unauthenticated `POST`
   requests to `/hooks/linear`. Linear cannot present an access token, and the
   signature check authenticates the request.
3. In Linear, go to **Settings > API > Webhooks** and create a webhook with the
   URL `https://<hk origin>/hooks/linear`.
4. Select teams `ROB` and `MGH`, and resource types **Comment** and **Issue**.
5. Copy the signing secret into `HANDOFFKEEP_LINEAR_WEBHOOK_SECRET` and restart
   the server.
6. Post a plain comment on a test issue. Linear's webhook page should show a 200
   delivery, and no document should appear.

Linear retries a failed delivery after 1 minute, 1 hour and 6 hours, and may
disable a webhook whose URL stays unresponsive. Re-enable it in the same page.
