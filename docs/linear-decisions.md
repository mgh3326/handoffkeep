# Operator decisions through Linear

The desk asks the operator a question as a Linear issue, and hk posts it to a
Discord channel. The operator answers on the issue with a short comment. The
Linear webhook records the answer in hk, and the desk is notified.

## Configuration

Set these on the server. The flow also needs the webhook from
[linear-webhook.md](linear-webhook.md), because answers arrive through it.

| Variable | Value |
|---|---|
| `HANDOFFKEEP_LINEAR_OPERATOR_USER_ID` | The Linear user id whose answers count. Unset turns the flow off: `decisions ask` is refused and the webhook handles dots only. |
| `HANDOFFKEEP_LINEAR_DECISION_TEAM` | Team key for new issues. Default `ROB`. |
| `HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL` | A Discord incoming webhook, `https` only. Unset disables channel posts; everything else keeps working. |
| `HANDOFFKEEP_LINEAR_DECISION_STATES` | Optional, for answering by state change. See [Answer by state](#answer-by-state). |
| `HANDOFFKEEP_NOTIFICATION_FORWARD` | `on` sends `notification_outbox` rows to the hub. See [Desk notification](#desk-notification). |

The team needs a label named `decision` and a state of type `canceled`. Without
them, `decisions ask` is refused with `linear_decision_target_missing` and
writes nothing.

Keep the Discord webhook URL out of the repo: anyone holding it can post to the
channel.

### Discord

hk posts to the channel itself. Linear's Discord integration does not do this:
connecting it (**Settings > Integrations > Discord**) adds Linear's slash
commands to the server, but it posts nothing to a channel when an issue is
created, and it has no label filter.

To set up the channel, create an incoming webhook on it in Discord, then set
`HANDOFFKEEP_DISCORD_DECISIONS_WEBHOOK_URL`.

| Event | Message |
|---|---|
| Issue created | Question, numbered options, `ver`, deadline, how to answer, Linear link |
| Answered on any path | `✅`, the chosen option, `ver`, Linear link |
| Withdrawn or default applied | `⏹️`, how it closed, `ver`, Linear link |
| Superseded | `♻️`, the new decision, Linear link |

A long question is cut to fit Discord's 2000-character limit; the issue has it
in full. Messages cannot ping anyone, whatever the question says. Posts made
while the URL is unset are skipped for good and are not replayed when you set it.

## How the desk asks

```bash
handoffkeep decisions ask 2615 \
  --question 'Ship the migration this week?' \
  --option 'A|Ship now' --option 'B|Wait a week' --recommended A \
  --default-action 'Hold' --default-option B \
  --due 2026-11-03T18:00:00+09:00 \
  --ref handoffkeep#107
```

The flags match `tasks decision-request`, plus a repeatable `--ref`. The API is
`POST /v1/tasks/{id}/decision-request/ask` with the same body as
`/decision-request`. It answers `201` when it created the issue and `200` when
there was nothing left to create.

The command records the request and creates one issue in the team, labelled
`decision` and titled `[D-<task>-<revision>] <question>`. The body has the
numbered options, default, deadline, refs and `ver`.

`ver` is the first 8 hex digits of the SHA-256 of the question, options and
refs. It binds an answer to exactly what was asked.

**Retries are safe.** Re-send the same command after any failure. It finds the
same request, and it creates an issue only if the request has none. The issue id
derives from the request id, so a crash between creating the issue and saving
the link cannot produce a second issue. A `503 linear_unavailable` means the
request may be recorded without its issue; re-send to finish.

**To change a question,** name the open request:

```bash
handoffkeep decisions ask 2615 --question 'Ship it next week?' ... --supersedes dr-2615-1
```

Without `--supersedes`, a different question on a task with an open request is
refused with `decision_request_open`. With it, the new request gets a new `ver`
and a new issue. The old issue gets a comment pointing to the new one and moves
to the team's canceled state.

**Do not ask on a task under an activated wrk job.** Those tasks refuse any write
without a wrk receipt, and a webhook has none, so an answer on Linear would be
refused and the issue would say so. Use a task of the desk's own.

## How the operator answers

Comment on the issue. Only the first line is read, and it must be one of:

| First line | Means |
|---|---|
| `1` | Option 1 |
| `A` | Option A |
| `D-2615-1 1` | Option 1, and this must be decision D-2615-1 |
| `D-2615-1 ab12cd3e 1` | The same, and the question must still be `ver` `ab12cd3e` |
| `1 ver=ab12cd3e` | Option 1, and the question must still be `ver` `ab12cd3e` |

Put any note on later lines; it is kept in the audit record. Any other comment
is ordinary discussion, and hk ignores it without replying.

The first valid answer wins. hk replies on the issue:

| Situation | Reply | Request |
|---|---|---|
| First valid answer | `✅ recorded: ...` | Resolved |
| Later answer | `already resolved: ...` | Unchanged |
| Superseded issue | `⚠️ not recorded: ... superseded ...` | Unchanged |
| Wrong `D-<id>` or `ver`, no such option, closed issue, finished task, or protected job | `⚠️ not recorded: <reason>` | Unchanged |
| Comment from anyone but the operator | None | Unchanged |

An answer after the deadline still counts while the request is open: a passed
deadline changes nothing in hk until someone records that the default was
applied.

Replies are queued in the same transaction as the answer and retried with
backoff, so a Linear outage delays a reply but does not lose it. After 8 failed
attempts the row stays in `linear_decision_ops` with state `failed` and its
`last_error`.

### Who counts as the operator

A comment counts only when all of these hold:

- Its author is `HANDOFFKEEP_LINEAR_OPERATOR_USER_ID`.
- It has no bot actor.
- The webhook's actor, when present, is that same user with type `user`.

**Limit:** a client using the operator's personal API key or the operator's OAuth
connection may look identical to the operator. Do not give agents credentials
that act as the operator's Linear user, or their comments can answer decisions.

### Answer by state

This is optional and off by default. Comments are the supported way to answer.

To turn it on, map state names to option numbers:

```bash
HANDOFFKEEP_LINEAR_DECISION_STATES='Approved=1,Rejected=2'
```

The states must exist in the team. Moving an issue into a mapped state answers it
when the operator made the move and every option of the request has a state. In
this example that means requests with exactly two options. On any other request
hk replies that the operator should comment instead. Later edits to an issue
resting in that state do not count.

## Audit record

The answer is stored in the task's `refs.decision_request.resolution`, and each
write is kept in the task's `task_events` snapshot. `resolution.at` holds when hk
recorded it. `resolution.linear` holds:

| Field | Value |
|---|---|
| `decision_id` | `D-<task>-<revision>` |
| `ver` | The `ver` the issue asked |
| `choice` | The choice as answered: a number or a key |
| `raw_text` | The comment body, or `state: <name>`, up to 2000 bytes |
| `user_id` | The operator's Linear user id |
| `source` | `comment` or `state` |
| `comment_id` | The comment that answered, for a comment |
| `issue_id` | The Linear issue |
| `delivery_id` | The `Linear-Delivery` header of the webhook delivery |
| `event_at` | Linear's timestamp for the comment or the state change |

## Desk notification

An answer writes one `notification_outbox` row for lane `operator-desk`, kind
`decision_answered`, event id `dr-<task>-<revision>-answered`.

With `HANDOFFKEEP_NOTIFICATION_FORWARD=on`, the server sends every unsent row to
the hub at `POST /v1/relay/events`, using `HANDOFFKEEP_HUB_URL` and
`HANDOFFKEEP_HUB_TOKEN`. It checks every 5 seconds and stores the row id the hub
returns as the sent receipt. A failed row is retried with backoff, from 30 seconds
up to 10 minutes, and does not hold up other rows.

| `kind` | Lane-event type |
|---|---|
| `dots_decision`, `decision_answered` | `decision` |
| `dots_report` | `report` |
| `dots_followup`, `chat_answer` | `info` |

Each row goes to its own `target_lane`. The hub dedupes on lane and event id, so
a retry, or a poller sending the same event, still delivers once.

**Before turning this on,** check the backlog with `GET /v1/assistant/outbox`. The
first run sends every row still unsent, however old, and nothing has drained this
outbox before.

## Set up the webhook

Enable both resource types on each of the two existing webhooks, in Linear's
**Settings > API > Webhooks**:

- **Comment**, which carries answers and dots replies
- **Issue**, which carries state changes and is used only with
  `HANDOFFKEEP_LINEAR_DECISION_STATES`

Webhook registration is covered in [linear-webhook.md](linear-webhook.md).
