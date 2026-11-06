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
| `HANDOFFKEEP_CONSOLE_URL` | The console's base URL, `https` only, for example `https://console.example`. A request's message links to `<base>/ui/decisions/<id>`, where `<id>` is the `decision_requests` id; the digest links to `<base>/ui/decisions?tab=open`. Unset leaves the link out, and the message says to look on the console's Decisions page or run `handoffkeep decisions list`. A value that is not an `https` URL stops `serve` at startup. |
| `HANDOFFKEEP_DISCORD_OPERATOR_USER_ID` | The operator's Discord user id (17 to 20 digits). A pressing request's first post mentions this user. Unset mentions nobody. Any other value stops `serve` at startup; the error does not quote the value. |

## Messages

| When | Message |
|---|---|
| A request opens | `❓`, the request id, source, lane, task id and title when present, the question, when it was asked and how long it has been open, the due date when present, and the console link. A long question is cut to fit Discord's 2000-character limit. |
| Answered | The same message, edited: `✅`, who answered, when, and the answer. |
| Superseded | `♻️`, what superseded it, and when. |
| Expired | `⌛` and when it expired. |
| Daily digest | Requests open more than 24 hours, oldest first, one line each, with a `+N more` line when they do not all fit in one message. |

An edited message keeps the request id, source, lane, task and link, and drops
the question. `@everyone` and role mentions never ping: `allowed_mentions`
sets `parse` empty on every post and edit, so `@everyone` in a question is
plain text.

### Operator mention

When `HANDOFFKEEP_DISCORD_OPERATOR_USER_ID` is set, the first post of a
pressing request starts with `<@id>` and its `allowed_mentions` lists that one
user (`{"parse":[],"users":["<id>"]}`). A request is pressing when any of these
holds at the time of its first post:

- its task's `awaiting_decision` flag is set (asked with `--block`);
- it is a `task_request` whose task's current `refs.decision_request` is this
  request and has `human_only` set;
- its due time is within 2 hours, or already past.

Edits never mention, and a request that becomes near-due after its first post
is not mentioned again. Messages from the Linear flow never mention anyone.

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

The transaction that opens a request also inserts its `decision_deliveries`
row, one per destination (`discord` is the only one), so a message is owed
exactly when the request exists. A redelivered write finds the request already
recorded and queues nothing. Closing a request writes only the request row.

| Column | Meaning |
|---|---|
| `request_id`, `destination` | The primary key. |
| `message_id` | The posted message, for edits. |
| `desired_version` | The version the worker last tried to apply. |
| `applied_version` | The version the destination shows; empty before the first post. |
| `state` | `pending` while the worker owns the row; `unsent` and `failed` are final. |
| `attempts`, `next_attempt_at` | Retries of the current version. |
| `last_error_code` | The last send error's code (its text before the first `:`). |
| `posted_at`, `edited_at`, `created_at` | When. |

A version is the request's effective status: `open`, `answered`, `superseded`
or `expired`. It is not a stored counter, because expiry has no write of its
own. A pending row is due when its next attempt has come and
`applied_version` differs from the request's effective status; the worker in
`serve` then sets `desired_version` to that status, sends, and on success sets
`applied_version` to it. That is how an expiry reaches the channel.

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

The webhook URL is a credential. No error, log line or stored
`last_error_code` contains it.

`handoffkeep decisions hygiene` prints, per destination, how many rows are
`pending`, `lagging` (pending, behind the request, and more than an hour past
their next attempt), `unsent` and `failed`:

```json
{"destinations":{"discord":{"pending":12,"lagging":0,"unsent":0,"failed":1}}}
```

It reads `GET /v1/decisions/deliveries/hygiene`.

## Rollout

Schema version 38 adds two new tables, `decision_notify` and
`decision_notify_digest`. It locks no existing table and writes no rows.
Requests that were already open before the upgrade get no message of their
own. The digest lists them once they are more than a day old.

Schema version 48 adds `decision_deliveries` and copies every
`decision_notify` row into it under an `EXCLUSIVE` lock on `decision_notify`
(so an older worker still sending finishes before the copy, and none starts
during it): a
`last_status` of `open`, `answered`, `superseded` or `expired` (or empty)
becomes `applied_version` on a `pending` row, and `unsent` and `failed` keep
that state. `decision_notify` stays in place, unwritten, for one release.
