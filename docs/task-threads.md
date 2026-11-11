# Task threads in Discord

Each hk task in a configured project gets one post in a Discord forum
channel when it starts. Each later record about the task adds one short
Korean line to that post. The design is hk:doc
`design/2026-11-11/operator-surfaces` §1 (hk 3990).

Discord is a view of hk. hk posts through a webhook. There is no bot, and hk
does not read anything back from Discord. A reply typed in a thread is not an
instruction, and hk does not store it. To answer a decision, use the hk
console or MCP. hk stores no message text. It renders every line from the hk
record that the line describes, so hk can post a lost thread again.

## Configuration

Set these variables on the server.

| Variable | Value |
|---|---|
| `HANDOFFKEEP_DISCORD_TASKS_WEBHOOK_URL` | The webhook of a forum channel, `https` only. If unset, the poster does not run. A value that is not `https` stops `serve` at startup. The error does not quote the value. |
| `HANDOFFKEEP_DISCORD_TASKS_PROJECTS` | A comma-separated list of task projects that get threads. The default is `panewire,handoffkeep,mergewake,rstack`. |
| `HANDOFFKEEP_DISCORD_OPERATOR_USER_ID` | The same variable that the decisions channel uses ([decision-discord.md](decision-discord.md)). The mentions below ping this user. If unset, no message pings anyone. |
| `HANDOFFKEEP_CONSOLE_URL` | The same variable that the decisions channel uses. Links go to `<base>/ui/tasks/<id>` and `<base>/ui/decisions/<id>`. |

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
minute to the webhook. When Discord answers 429, hk waits for the
`Retry-After` time before it sends anything else. After a task settles, its
thread gets new lines for 30 more days.

## Delivery and recovery

`task_thread_deliveries` has one row per task and destination. Before each
send, hk claims the row and commits the claim. After the send, hk records the
result in a second transaction. The thread id is stored in the same
transaction that records a successful create. The create uses `?wait=true`,
so a 2xx answer contains the new thread.

| State | Meaning |
|---|---|
| `pending` | A thread is owed and is not yet created. |
| `creating` | A create is claimed and in flight. |
| `active` | The thread exists. Lines are posted to it. |
| `needs_operator` | A create may have reached Discord but its answer was lost. Either the request timed out, or the worker died after Discord's 2xx and before the commit. A webhook cannot list a forum's threads (only a bot can), so hk cannot find out whether the thread exists. hk posts nothing more for the task, and it never creates a second thread. |
| `failed` | Discord refused the request with a 4xx other than 429, for example because the webhook or the thread was deleted, or the retries ran out (about a day). |

A 5xx answer, a 429 answer or a failed connection means Discord did not take
the message, so hk sends it again later. If a line's outcome is unknown (a
timeout, or a claim older than two minutes left by a dead worker), hk counts
the line as posted and does not send it again. A line can be lost this way,
but it is never posted twice.

A deploy failure is posted only when it is a `deploys` row (written by
`StartDeploy` and `FinishDeploy`) whose `deploy_changes` include a change of
the task. A failure recorded only as a deploy-record document links to no
task, so it is not posted.

## Rollout

1. The operator creates the forum channel and its webhook.
2. Store the webhook URL in the server environment as
   `HANDOFFKEEP_DISCORD_TASKS_WEBHOOK_URL`. Use the `hk-infisical` wrapper
   (hk:doc `reference/infisical-fleet`), and do not print the value.
3. Redeploy.
4. Watch one task in a listed project start, and check that its post appears.
