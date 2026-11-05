# Decision requests in Discord

Every open decision request (`decision_requests`, see
[linear-internalize.md](linear-internalize.md#decision-requests)) gets one
message in the decisions channel. When the request is answered, superseded or
expires, hk edits that message instead of posting a new one. Once a day, one
digest lists the requests that have been open for more than 24 hours.

This is outbound only. hk posts and edits through the incoming webhook it
already uses for [Linear decisions](linear-decisions.md#discord). There is no
bot, no inbound route and no new credential. To answer a request, use the
console or the CLI, as before.

## Configuration

Set these on the server.

| Variable | Value |
|---|---|
| `HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL` | The decisions channel's incoming webhook, `https` only. This is the variable the Linear flow uses. Unset sends nothing. |
| `HANDOFFKEEP_CONSOLE_URL` | The console's base URL, `https` only, for example `https://console.example`. Messages link to `<base>/ui/decisions`. Unset leaves the link out, and the message says to look on the console's Decisions page or run `handoffkeep decisions list`. A value that is not an `https` URL stops `serve` at startup. |

## Messages

| When | Message |
|---|---|
| A request opens | `❓`, the request id, source, lane, task id and title when present, the question, when it was asked and how long it has been open, the due date when present, and the console link. A long question is cut to fit Discord's 2000-character limit. |
| Answered | The same message, edited: `✅`, who answered, when, and the answer. |
| Superseded | `♻️`, what superseded it, and when. |
| Expired | `⌛` and when it expired. |
| Daily digest | Requests open more than 24 hours, oldest first, one line each, with a `+N more` line when they do not all fit in one message. |

An edited message keeps the request id, source, lane, task and link, and drops
the question. No message can ping anyone: `allowed_mentions` is empty on every
post and edit, so `@everyone` in a question is plain text.

The first post waits one minute after the request opens. A request that closes
in that minute is never posted.

## Requests asked through Linear

Requests with source `linear` are not posted here. The Linear flow already
announces them ([linear-decisions.md](linear-decisions.md#discord)). Asking
through Linear first opens a `task_request` row, and recording the Linear issue
supersedes that row within seconds. Because of the one-minute wait, that row
closes before its first post, so it is never posted either. If creating the
issue fails, the `task_request` row stays open and is posted here, so the
question is still seen.

The digest lists every open request older than 24 hours, whatever its source,
`linear` included. Snoozed requests are left out.

## Delivery

The transaction that opens a request also inserts its `decision_notify` row,
so a message is owed exactly when the request exists. A redelivered write
finds the request already recorded and queues nothing. Closing a request
writes only the request row. The worker in `serve` compares the request's
status with the status the message shows (`last_status`) and edits the message
when they differ. Expiry has no write of its own, and it reaches the channel
the same way.

The worker checks every 10 seconds. A failed post or edit is retried with
backoff (30 seconds, doubling to an hour) for about a day, then the row stays
`failed`. A 4xx answer other than 429 is not retried. For example, Discord
answers 404 when someone deleted the message. A post that gets no message id
back is also not retried, because retrying would post a second message.
Sending never blocks or fails the write that opened or closed a request.

Without the webhook, every message that falls due settles as `unsent`, and so
does the day's digest. Setting the webhook later does not replay them.

The digest goes out on the worker's first check after 00:00 UTC. A
`decision_notify_digest` row per UTC day makes sure only one goes out, even
with several servers. A day with nothing older than 24 hours sends nothing.

The webhook URL is a credential. No error, log line or stored `last_error`
contains it.

## Rollout

Schema version 37 adds two new tables, `decision_notify` and
`decision_notify_digest`. It locks no existing table and writes no rows.
Requests that were already open before the upgrade get no message of their
own. The digest lists them once they are more than a day old.
