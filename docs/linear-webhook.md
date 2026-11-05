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

A repeated delivery answers `duplicate` and writes nothing. If a Linear API read
fails, the endpoint answers `503 linear_unavailable` so that Linear retries.

### Document

The key is `dots/results/<ISSUE>/<YYYY-MM-DD>-<first 8 of comment id>`, with the
date taken from the comment's `createdAt`. The document has kind `note` and
session `operator-desk`. The first writer of a key wins.

### Notification

Each notification is one `notification_outbox` row for lane `operator-desk`,
written in the same transaction as its document. Read unsent rows with
`GET /v1/assistant/outbox`, and record a hub receipt with
`POST /v1/assistant/outbox/sent`. The server can do both itself: see
[Desk notification](linear-decisions.md#desk-notification).

| `kind` | Lane-event type | `event_id` |
|---|---|---|
| `dots_report` | `report` | `dots-<comment id>` |
| `dots_decision` | `decision` | `dots-<comment id>` |
| `dots_followup` | `info` | `dots-followup-<comment id>` |

The poller uses the same event ids, so the hub dedupes a notification sent by
both paths. The handler adds no row when the poller already wrote the document,
because the poller has already notified.

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
