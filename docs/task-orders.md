# Task orders (hk 5125)

An order is the operator's "set going" for one task that has not started. It
records the operator's words, the channel and the time, and hk delivers it to
the lane that owns the task's project. hk starts no worker (decision 390 is
unchanged); the lane reads the order from the record, acknowledges it, and
settles, briefs and spawns as before. Design: `design/2026-11-18/hk5112-track-b`
§5 and §8.1.

## Invariant table

| # | Invariant | Input | Transition | Enforced in | Observable result |
|---|---|---|---|---|---|
| 1 | **An order starts work and widens nothing.** It names an existing task; the words are the operator's instruction for that task only. No approval (merge, deploy, spending, secret, policy) rides on it. | task id, words | none beyond the order row, one task event and one outbox row | `CreateTaskOrder` writes only those three rows; the outbox text says "no approval" and the task's state, lane, refs and decision requests are never touched | task row unchanged after an order; no decision request opened or answered |
| 2 | **Only operator principals write an order.** Console Access (verified), the operator bearer (verified), or dots relaying the operator's words (`verified=false`). Every other client is refused. | caller identity | refused before the store is called | handlers: `/v1` route and MCP tool check the caller; the store ties `verified` to the channel (`dots` is never verified, `console` and `cli` always are) and refuses a mismatch | other bearer clients get 403 `operator_required`; `task_orders.verified` and `channel` agree (CHECK) |
| 3 | **Idempotent write.** A key is required. The same actor, key, task and words replay the first order; the same key with other words or another task is refused. | actor, idempotency_key, task id, words | replay returns the stored order and writes nothing | UNIQUE (actor, idempotency_key) and an in-transaction compare, checked before the task-state and lane checks so a replay survives the task starting later | second call returns `duplicate=true` and the same order id; one row, one event, one outbox row; other words give 409 `order_key_conflict` |
| 4 | **Only a not-yet-started task.** State must be `backlog`, `unstarted` or `triage`. | task state | refused otherwise | `CreateTaskOrder` under the task row lock | 409 `order_task_started` with the state as reason; nothing written |
| 5 | **Target lane.** The project's `design_lane`, else the task's lane, else refused. | task project, task lane | `target_lane` fixed at write time | `CreateTaskOrder` reads `task_projects.design_lane` in the same transaction | order's `target_lane` equals the design lane, or the task lane; 409 `order_no_lane` when both are empty |
| 6 | **One transaction.** The order row, the task event and the outbox row commit together or not at all. | - | one transaction under the task-mutation lock and the task row lock | `CreateTaskOrder`, `AckTaskOrder`, `WithdrawTaskOrder` each use one `begin` | a forced failure after the order insert leaves no order, no event and no outbox row |
| 7 | **Delivery.** The outbox row has kind `task_order`, event id `task-order-<id>`, and goes to the target lane as hub type `order`. The text carries the order id, task id and title, the words and the channel. | outbox row | forwarded by `hubforward` | `notifyType` maps `task_order` to `order`; `notification_outbox_kind_check` admits the kind on both dialects | forwarder posts `type=order`; text contains the id, task, title, words, channel |
| 8 | **Acknowledge.** The target lane records `acked_at`, the acker and a comment; a task comment is written. Ack is not an approval and changes no task state. | order id, comment | open -> acked, once | `AckTaskOrder` under the row lock; the second ack replays and writes nothing | `acked_at` set once; one task comment; replay is `duplicate=true` |
| 9 | **Withdraw.** Only an operator principal, with a reason, on an open order. It posts its own notice (event id `task-order-withdrawn-<id>`) to the same lane. | order id, reason | open -> withdrawn, once | handler principal check; `AckTaskOrder`/`WithdrawTaskOrder` share the open-state check; CHECK forbids acked and withdrawn together | `withdrawn_at` set; second notice never written; withdraw of an acked order is 409 `order_not_open` |
| 10 | **The lane wait clears exactly once.** An order waits on its target lane while it is open (not acked, not withdrawn). Acking or withdrawing clears it, and nothing else does. | order state | open -> closed | `waits_on_lane` is derived from the row (open orders only), so no second store exists to reconcile; the two closing writes are mutually exclusive | `waits_on_lane` is the lane while open and empty after either close; B-4 reads it |
| 11 | **Both dialects.** `task_orders` exists with the same constraints on PostgreSQL and MySQL, the outbox kind check admits `task_order` on both, and the copy registry carries the table. | - | migration `20261118-task-orders` and its MySQL twin | `migration_task_orders.go`, `migration_mysql_task_orders.go`, `copyTableRegistry` | the area cases run on both dialects; `TestMySQLMigrationRegistryCoversNewPostgresMigrations` passes |
| 12 | **Expand-only.** The migration adds a table and widens a check; the previous release keeps serving. | - | - | the new table is unused by the previous release; widening `notification_outbox_kind_check` rejects nothing it accepted | `scripts/test-previous-release-schema.sh` still passes |

Surfaces over the one store method set: `POST /v1/tasks/{id}/orders`,
`POST /v1/task-orders/{order}/ack`, `POST /v1/task-orders/{order}/withdraw`,
`GET /v1/tasks/{id}/orders`, MCP `task_order` / `task_order_ack`, CLI
`tasks order`, `tasks order-ack`, `tasks order-withdraw`, and the console
action 진행시키기 on a backlog task.
