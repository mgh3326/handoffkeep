# Set up the Linear fleet work board

The board mirrors hk tasks into team ROB. hk keeps the task and event records.
The issue shows the executing sessions, the PR and head, review results, and the
latest event.

## Enable the board

1. Configure the fleet-agent OAuth app credentials described in
   [Linear authentication](linear-webhook.md). The board uses the same
   `LinearAPI` client as the decision and webhook paths.
2. Set `HANDOFFKEEP_LINEAR_WORK_BOARD=on` on the hk server.
3. Set `HANDOFFKEEP_LINEAR_WORK_BOARD_BASE_URL` to the public origin of the hk
   console. The managed description links to `/ui/tasks/<id>` on this origin.
   Without this setting, the description carries the task id and CLI command.
4. Check that ROB has the workflow states `Backlog`, `In Progress`, `In Review`,
   `Done`, and `Canceled`. The writer resolves their ids by name.
5. Deploy the hk change through the desk's normal process.
6. Deploy the runtug producer for [the placement contract](placement-decisions.md).

The board creates its `fleet` label and metadata labels when needed. The OAuth
app needs permission to read and write issues, labels, and comments in ROB.
The board can run without the Linear webhook or an operator user id.

A task's first claim or placed spawn queues its issue. If its title contains
`ROB-1234`, hk reuses that issue. Otherwise hk creates an issue with a stable id
derived from the task id. The task's `refs.linear.issue_id` and
`refs.linear.identifier` record the link. The issue description links back to hk.

## Build a host view

1. Create a custom issue view in team ROB.
2. Add the `fleet` label filter.
3. Select **Display options**, then **Group by**, then **Label**.
4. Keep the `host:` groups expanded and collapse the other label groups.
5. Save the view as `Fleet by host`.

For one machine, add a label filter such as `host:host-a`. Keep completed issues
visible if you want to inspect recently merged work.

## Build a role view

1. Duplicate the host view.
2. Keep the `role:lead`, `role:worker`, `role:review`, and `role:consult` groups
   expanded.
3. Collapse the `host:`, `model:`, and `repo:` groups.
4. Save the view as `Fleet by role`.

Use ordinary labels for these values. A running review adds the reviewer's host,
model family, and role alongside the lead's labels. Putting these labels into a
single-select label group would remove that information. Label grouping can
show the same issue in several groups. Linear documents
[display grouping](https://linear.app/docs/display-options) and
[custom views](https://linear.app/docs/custom-views).

## Check an issue after deployment

1. Claim a disposable hk task with a job id.
2. Record a placed spawn through `POST /v1/placements` using the node credential.
3. Open the task's `refs.linear.identifier` in ROB.
4. Check the `fleet`, `host:`, `role:`, `model:`, and `repo:` labels.
5. Check that the Fleet work section links back to the task and shows its pane.
6. Send `[<job-id>] READY | <PR and head>` through the existing lane-event path.
7. Check that the issue moves to `In Review` and receives one READY comment.
8. Redeliver that same event id and check that no second comment appears.
9. Record a review placement against the owning task. Check both sessions' labels.
10. End the review with a FINDINGS event containing its count and verdict.
    Check the review history and the removal of the active review labels.

Record the issue identifier and event id in the deployment report. This live
check belongs to the desk after deployment.

## Inspect or repair a stale issue

1. Check the task, its placement rows, and its relay events in hk.
2. Inspect `linear_outbox` rows with `op='work_board'`. A pending row's
   `last_error` identifies an API outage, missing state, or malformed section.
3. Restore the missing Linear permission, state, or section boundary.
4. Wait for the row's `next_attempt_at`. The writer retries with the same issue
   and comment ids.

The source writers update the projection and outbox in the same transaction.
The outbox worker checks due writes every five seconds. It does not scan tasks
or placement sources to discover changes. An hourly reconciler resends the
projection, current task fields, placement records, and job completion details.
It also recovers placements recorded while the board was disabled. It adds no
comments. Merged and dropped tasks need another repair only when their projected
state or update time differs from the task. Unrelated issue text and labels
remain in place.

The managed description boundaries are `<!-- hk-work-board:start -->` and
`<!-- hk-work-board:end -->`. Keep both boundaries when editing the issue.
Ambiguous or incomplete boundaries leave the outbox row pending instead of
removing operator text.
