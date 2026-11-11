# Task threads in Discord

Each hk task in a configured project gets one post in a Discord forum
channel when it starts. Each later record about the task adds one short
Korean line to that post. The design is hk:doc
`design/2026-11-11/operator-surfaces` §1 (hk 3990).

Discord is a view of hk. hk posts as a bot (hk 4040), and the bot does only
three things: it creates posts in the forum, posts lines into their threads,
and lists the forum's threads to find a post whose create answer was lost
(below). It reads no message content and takes no instruction from Discord. A
reply typed in a thread is not an instruction, and hk does not store it. To answer a decision, use the hk
console or MCP. hk stores no message text. It renders every line from the hk
record that the line describes, so hk can post a lost thread again.

## Configuration

Set these variables on the server.

| Variable | Value |
|---|---|
| `HANDOFFKEEP_DISCORD_BOT_TOKEN` | The bot's token, a secret. hk sends it as `Authorization: Bot <token>` to `https://discord.com/api/v10` and never logs it or quotes it in an error. |
| `HANDOFFKEEP_DISCORD_TASKS_FORUM_ID` | The id of the forum channel (digits only). |
| `HANDOFFKEEP_DISCORD_TASKS_PROJECTS` | A comma-separated list of task projects that get threads. The default is `panewire,handoffkeep,mergewake,rstack`. |
| `HANDOFFKEEP_DISCORD_OPERATOR_USER_ID` | The same variable that the decisions channel uses ([decision-discord.md](decision-discord.md)). The mentions below ping this user. If unset, no message pings anyone. |
| `HANDOFFKEEP_CONSOLE_URL` | The same variable that the decisions channel uses. Links go to `<base>/ui/tasks/<id>` and `<base>/ui/decisions/<id>`. |

Set both of the first two variables, or neither. If both are unset, the
poster does not run. If only one is set, or the forum id is not a Discord id,
`serve` stops at startup. The error names the variable and does not quote
the value.

When the poster runs for the first time, it records the newest task event.
Tasks that started before that event get no thread, so hk posts no backlog
when you turn the poster on.

## What is posted

| Record | Line |
|---|---|
| The task's first move into a started state (researching, developing or reviewing) from outside that category | A new forum post named `#<id> <title>`, cut to 100 characters. The first message shows the project, kind, grade and tier (when set) and the console link. |
| A `mergewake.pr` event | `🔀 PR 열림` and the PR link |
| A review gate verdict (`changes.gate_state` ready or blocked, for each new head and state) | `✅ 리뷰 판정: 통과` or `⛔ 리뷰 판정: 차단`, the counts of open findings by severity, and the PR link |
| A move to merged or deployed | `🟣 머지됨` or `🚀 배포됨` and the PR link |
| A deploy attempt that failed or was rolled back, linked to the task through `deploy_changes` and `change_tasks` | `💥 배포 실패` or `↩️ 배포 롤백`, the service and the deploy id |
| A `mergewake.revert` event | `⏪ 리버트됨` and the link to the revert PR |
| A decision request that opens on the task | `❓ 결정 요청 #<id> 열림` and the link to its console page (the question is not copied) |
| A decision request that is answered | `🗳️ 결정 요청 #<id> 답변됨`, who answered, the channel from `decision_resolutions` (when it is set), the time, and the link |
| A move to completed, dropped, duplicate or icebox | `🏁 완료`, `🗑️ 드롭됨`, `🔁 중복으로 종료` or `🧊 보류(icebox)로 이동` |

Lines contain only structured fields and links. hk never posts a comment,
report, finding or question body. Every message sets the `SUPPRESS_EMBEDS`
flag, so a PR link does not unfold into the PR description. The operator is
mentioned only for a blocked verdict that has critical or high findings
open, a failed or rolled-back deploy, and an opened decision.

Lines go out in the order of their records. hk sends at most 30 messages per
minute to the forum. When Discord answers 429, hk waits for the
`Retry-After` time before it sends anything else. After a task settles, its
thread gets new lines for 30 more days.

## Delivery and recovery

`task_thread_deliveries` has one row per task and destination. Before each
send, hk claims the row and commits the claim. After the send, hk records the
result in a second transaction. The thread id is stored in the same
transaction that records a successful create. A create is
`POST /channels/{forum}/threads`, and a 2xx answer is the new thread. A line
is `POST /channels/{thread}/messages`, and a 2xx answer is the new message.

| State | Meaning |
|---|---|
| `pending` | A thread is owed and is not yet created. |
| `creating` | A create is claimed and in flight. Or, with no claim, a create's outcome is unknown and hk will look for its thread (below). |
| `active` | The thread exists. Lines are posted to it. |
| `needs_operator` | A create's outcome is unknown and the lookup could not settle it: the lookup failed (other than a 429 or a failed connection), was refused or was incomplete, or more than one thread matches. Or the outcome stayed unknown, or the lookup could not run, for 32 attempts. hk posts nothing more for the task, and it never creates a second thread. |
| `failed` | Discord refused the request with a 4xx other than 429, for example because the webhook or the thread was deleted, or the retries ran out (about a day). |

A 429 answer or a failed connection means Discord did not take the message,
so hk sends it again later. Any other outcome is unknown: a 5xx answer (a
gateway can answer 502 after Discord made the post), a timeout, or a claim
older than two minutes left by a dead worker. hk counts such a line as posted
and does not send it again. A line can be lost this way, but it is never
posted twice.

A create with an unknown outcome may have made the thread. hk does not create
it again blind. It waits two minutes after the unknown outcome, so a slow
create can appear, and then it looks for the thread:

1. It reads the bot's user id (`GET /users/@me`) and the forum's guild
   (`GET /channels/{forum}`). It reads each once per process.
2. It lists the guild's active threads (`GET /guilds/{guild}/threads/active`)
   whose parent is the forum, and the forum's archived public threads
   (`GET /channels/{forum}/threads/archived/public`), page by page until
   Discord reports no more. Each page starts just after the last thread's
   archive time, so threads archived at the same instant are read again,
   not skipped. If the archived list does not end within 10 pages, or a page
   brings no new thread, the lookup counts as failed.
3. A candidate is a thread with exactly the task's thread name whose owner is
   the bot. A thread that someone else made, or that is in another channel,
   is not a candidate.

| Candidates | Result |
|---|---|
| Exactly one | The row becomes `active` with that thread. hk logs `discord.task_thread_recovered`. |
| None | hk creates the thread again. |
| More than one, or the lookup fails | The row becomes `needs_operator`. |

A lookup that Discord answers with 429, or that cannot connect, did not run.
hk waits for `Retry-After` (or the backoff) and looks again; it does not
create in the meantime.

A deploy failure is posted only when it is a `deploys` row (written by
`StartDeploy` and `FinishDeploy`) whose `deploy_changes` include a change of
the task. A failure recorded only as a deploy-record document links to no
task, so it is not posted.

## Rollout

1. The operator creates the forum channel. The bot is the existing write
   bot; a separate bot comes later only if it is needed.
2. On the forum channel, the operator gives the bot these permissions: View
   Channel, Send Messages (Create Posts), Send Messages in Threads, and Read
   Message History.
3. Store the bot token as `HANDOFFKEEP_DISCORD_BOT_TOKEN` and the forum
   channel id as `HANDOFFKEEP_DISCORD_TASKS_FORUM_ID` in the server
   environment. Use the `hk-infisical` wrapper (hk:doc
   `reference/infisical-fleet`), and do not print the token.
4. Redeploy.
5. Watch one task in a listed project start, and check that its post appears.
