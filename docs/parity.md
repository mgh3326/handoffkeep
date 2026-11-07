# Feature parity: YouTrack / console / API / CLI / MCP

Operator rule (2026-11-07, hk:doc `design/2026-11-07/hk-dots-two-surfaces` §1 and §4):
YouTrack and the hk console are the operator's windows, and the ChatGPT dots (MCP)
are a second front-end on the same hk logic. Whatever the operator can do on the
web, dots must be able to do over MCP, and what dots writes must show on the web.
This page is the §4 matrix that manages that gap. A feature PR updates its own row.

The console write routes and the MCP tools that mirror them are also a table in
code: `ConsoleWrites` in
[`internal/parity/parity.go`](../internal/parity/parity.go). A test in that package
reads `internal/ui/api.go` and `internal/ui/ui.go` and fails when a console write
route has neither a mapped MCP tool nor a reasoned exception. `docs/parity.md` is
checked for every one of those routes, so this page cannot silently fall behind.

Baseline: `handoffkeep@64942ff`, the commit this matrix was taken from.

| Thing counted | Count at 64942ff |
| --- | --- |
| Console write routes | 14: 7 JSON routes in `internal/ui/api.go`, 7 HTML form routes in `internal/ui/ui.go` |
| MCP tool definitions | 22: 21 exposed on HTTP `/mcp`, 12 on stdio (the operator/meta tools are HTTP-only; `attachment_put_from_path` is stdio-only) |
| `/v1` write registrations | 81 method-scoped write registrations, plus four method-agnostic handlers that also accept writes: `/v1/checkpoints`, `/v1/documents/{key}`, `/v1/memory/{agent}/{name}`, `/v1/wrk/{path}` (the rows below cover the user-facing capabilities) |

Legend: **—** means the surface has no path for that capability. **ui.write** is
the console's log line `ui.write email= action= target= event_id= result=`
(`internal/ui/write.go`). **task_events** is the task event row (actor, from/to,
field, note, refs). **decision record** is the structured request answer with its
actor, channel and provenance. **outbox** is `youtrack_outbox` /
`youtrack_processed_activities`. "Operator" means a Cloudflare Access email
identity; "bearer" means an hk token client. Service Access identities may read
`/ui/api/*` and may set a host's accepting flag; the operator task, decision,
inbox and disposition writes refuse them.

| Group | Capability | YouTrack (3404 sync) | Console (/ui) | API (/v1) | CLI | MCP tool | Who may | Audit | Undo / recovery | Tests |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Tasks | Search, list and read a task | Mirrored issue is readable; Korean summary and operator comments are YouTrack-owned | GET /ui/api/board/tasks, GET /ui/api/board/tasks/{id}, GET /ui/api/search | GET /v1/tasks, GET /v1/tasks/{id}, GET /v1/tasks/export, GET /v1/search, GET /v1/triage | tasks list, tasks export, tasks show, tasks triage, ctx search | task_search, task_list, task_show | Operator, bearer, MCP bearer/Access | None (read) | n/a | internal/mcp/operator_test.go, internal/ui/board_*_test.go, internal/api/*task*_test.go |
| Tasks | Create a task (triage intake) | New issue is pulled into a triage task; unmapped projects are skipped | — | POST /v1/tasks, POST /v1/intake | tasks add, tasks intake | task_intake | Operator via YouTrack; bearer; MCP bearer/Access | created_by, task_origins (source/tenant/role), task_events | No delete; transition drop or duplicate | internal/api/intake_test.go, cmd/handoffkeep/intake_test.go, internal/mcp/operator_test.go |
| Tasks | Edit a filed task's title or body | YouTrack edits the Korean summary; hk mirrors it, it is not editable back | — | — | — | — | — | — | — | — |
| Tasks | State transition (claim, start, review, merge, deploy, complete, drop, hold, reopen) | hk 상태 pull applies TransitionTaskWith; a refusal reverts the field and comments; hk pushes its state | POST /ui/api/tasks/{id}/{action} (action = close-merged, close-completed, drop, hold, reopen; relane has its own route) | POST /v1/tasks/{id}/transition, POST /v1/tasks/{id}/reopen, POST /v1/tasks/{id}/claim, POST /v1/tasks/next | tasks transition, tasks reopen, tasks claim, tasks next | — | Operator (console/YouTrack); bearer (API/CLI) | task_events; ui.write; outbox and processed activities | reopen reverses a settled state; claim/start are not reversible | internal/ui/task_lifecycle_test.go, internal/api/task_closure_test.go, internal/youtrack/sync_test.go |
| Tasks | Priority | hk 우선순위 is pulled, pushed through the outbox, and reverted on refusal | POST /ui/api/tasks/{id}/priority | POST /v1/tasks/{id}/transition with a priority (only on a triage exit) | tasks transition --priority only on a triage exit; tasks add --priority | — | Operator (console/YouTrack); bearer on a triage exit | task_events field=priority; ui.write; outbox | Set again; YouTrack refusal reverts | internal/ui/task_priority_test.go, internal/api/youtrack_test.go |
| Tasks | Lane / relane | 레인 is pulled and pushed; a refused value is reverted with a comment | POST /ui/api/tasks/{id}/relane | POST /v1/tasks/relane (batch) | tasks relane | — | Operator (console/YouTrack); bearer (API/CLI) | task_events field=lane; ui.write; relane lane events; outbox | relane again; the console sends from_key so the move can be undone | internal/ui/board_relane_test.go, internal/api/relane_test.go, internal/youtrack/sync_test.go |
| Tasks | Project | hk project maps to a YouTrack project; a change moves the issue and keeps its stable id | — | POST /v1/tasks/{id}/project | tasks project | — | Operator via YouTrack; bearer | task_events kind=project | Set again | internal/api/task_project_test.go, internal/youtrack/sync_test.go |
| Tasks | Labels | — (not in the YouTrack field contract) | — | POST /v1/tasks/{id}/labels, GET/POST /v1/labels, PUT /v1/labels/{label}, POST /v1/labels/{label}/retire | tasks label, tasks labels | — | bearer | task_events field=labels | Remove the label; labels are never deleted, only retired | cmd/handoffkeep/task_model_test.go |
| Tasks | Due date | — | — | POST /v1/tasks/{id}/due | tasks due | — | bearer | task_events field=due | --clear | cmd/handoffkeep/task_model_test.go |
| Tasks | Parent / children | — | — (read on the task page) | POST /v1/tasks/{id}/parent | tasks parent, tasks add --parent | — | bearer | task_events | Re-parent or clear | cmd/handoffkeep/task_hierarchy_test.go, internal/store/hierarchy_test.go |
| Tasks | Relations | — | — (read on the task page) | POST /v1/tasks/{id}/relations, POST /v1/tasks/{id}/relations/remove | tasks relate, tasks unrelate | — | bearer | task_events | unrelate | cmd/handoffkeep/task_hierarchy_test.go, internal/store/hierarchy_test.go |
| Tasks | Split / move | — | — (move is relane) | Split: —; move: POST /v1/tasks/relane | Split: —; move: tasks relane | — | bearer | task_events | relane again | internal/api/relane_test.go |
| Tasks | Subscribers | — | — | POST /v1/tasks/{id}/subscribers | tasks subscribe, tasks unsubscribe | — | bearer | task row; no event | Unsubscribe | cmd/handoffkeep/task_model_test.go |
| Comments | Read comments | Operator comments are YouTrack-owned and copied to hk as operator:youtrack | GET /ui/api/board/tasks/{id}/comments | GET /v1/tasks/{id}/comments | tasks comments | task_show (last 20) | Operator, bearer, MCP bearer/Access | comment rows | n/a | internal/ui/routes_test.go, internal/mcp/operator_test.go |
| Comments | Write a comment | hk comments are pushed as issues comments; mirrored comments are linked so they do not echo | POST /ui/tasks/{id}/comments | POST /v1/tasks/{id}/comments | tasks comment | task_comment | Operator, bearer, MCP bearer/Access | comment.author; ui.write; YouTrack comment link | Append-only; no edit or delete | internal/ui/routes_test.go, internal/mcp/operator_test.go, internal/store/youtrack_test.go |
| Decisions | List open decisions | — | GET /ui/api/decisions, GET /ui/api/decisions/{id}, GET /ui/api/attention | GET /v1/decision-requests, GET /v1/decision-requests/sources, GET /v1/tasks/dispositions/summary | decisions list, decisions hygiene, decisions backfill | decisions_pending | Operator, bearer, MCP bearer/Access | None (read) | n/a | internal/ui/decision_requests_test.go, internal/mcp/operator_test.go |
| Decisions | Ask (create a structured request) | — | — | POST /v1/tasks/{id}/decision-request (the Linear /ask variant is separate) | tasks decision-request, decisions ask | — | bearer (agents and CLI) | decision record; task_events | No retraction; a superseded request can be replaced | cmd/handoffkeep/decision_request_test.go, internal/api/decision_records_test.go |
| Decisions | Answer a request | — | POST /ui/api/decision-requests/resolve | POST /v1/decisions/resolve, POST /v1/tasks/{id}/decision-request/resolve | decisions resolve, tasks decision-resolve | decision_answer | Console: operator only (operator_direct); bearer writes may be recorded as pending confirmation; MCP Access is operator_direct, MCP bearer follows the bearer rule | decision record (actor, channel, provenance); task_events; ui.write | Final; the request cannot be unanswered | internal/ui/decision_resolve_test.go, internal/api/decision_resolve_test.go, internal/mcp/operator_test.go |
| Decisions | Deny a pending confirmation | — | POST /ui/api/decision-requests/deny | — | — | — | Operator only | decision record; ui.write | The denied attempt is final; the request stays open | internal/ui/decision_deny_test.go |
| Decisions | Snooze a decision record | — | — | POST /v1/decision-requests/{id}/snooze | decisions snooze | — | bearer | decision record | Snooze again with a new time | internal/api/decision_records_test.go |
| Decisions | Legacy answer (task, escalation, lane) | — | POST /ui/decisions/answer, POST /ui/decisions/answer-batch | POST /v1/decisions/resolve | decisions resolve | — | Operator (console); bearer (API/CLI) | relay event; task_events; ui.write | Final; no undo | internal/ui/write_test.go, internal/api/decision_session_test.go |
| Inbox | Attention inbox read (decisions, stalled tasks, deploy risk) | — | GET /ui/api/attention | — | — | — | Operator only | Read state is stored per item | Inbox undo | internal/ui/dashboard_test.go |
| Inbox | Inbox state: read/unread, done, snooze | — | POST /ui/api/inbox/state | — | — | — | Operator only | ui.write; inbox state row | POST /ui/api/inbox/undo | internal/store/operator_inbox_test.go, internal/ui/operator_write_test.go |
| Inbox | Inbox undo | — | POST /ui/api/inbox/undo | — | — | — | Operator only | ui.write; inbox state row | Restores the previous item states | internal/store/operator_inbox_test.go, internal/ui/operator_write_test.go |
| Inbox | Disposition answer (single) | — | POST /ui/dispositions/answer | POST /v1/tasks/dispositions/{id}/apply (accept) | tasks disposition | — | Operator (console); bearer (apply) | ui.write; disposition record; lane event | Record is final; renotify covers a failed notice | internal/ui/disposition_test.go |
| Inbox | Disposition batch accept | — | POST /ui/dispositions/accept-batch | — | — | — | Operator only | ui.write per lane; disposition records | Final; individual records remain | internal/ui/disposition_test.go |
| Inbox | Disposition renotify | — | POST /ui/dispositions/renotify | — | — | — | Operator only | ui.write | Repeatable until notified | internal/ui/disposition_test.go |
| Documents | Read and browse documents | — | GET /ui/api/board/doc, GET /ui/api/docs, GET /ui/api/docs/prefixes, GET /ui/doc/{key} | GET /v1/documents, GET /v1/documents/{key} | doc get, doc list | get_document, list_documents, search_context | Operator (console doc pages refuse service identities); bearer; MCP bearer/Access | None (read) | n/a | internal/ui/docs_test.go, internal/mcp/server_test.go |
| Documents | Write a document | — | — | PUT /v1/documents/{key} | doc put, doc import | put_document | bearer, MCP bearer/Access | Document row (created_by, updated_at); no generic audit | Overwrite; no history | cmd/handoffkeep/doc_get_test.go, cmd/handoffkeep/manifest_test.go, internal/mcp/server_test.go |
| Context | Search across context, documents and tasks | — | GET /ui/api/search | GET /v1/search | ctx search | search_context | Operator, bearer, MCP bearer/Access | None (read) | n/a | internal/mcp/server_test.go, internal/store/search_tasks_test.go |
| Context | Checkpoints | — | — (timeline shows relay events, not checkpoints) | GET/POST /v1/checkpoints | ctx checkpoint, ctx recent | put_checkpoint, recent_checkpoints | bearer; MCP stdio/HTTP | checkpoint rows | Checkpoints are append-only | internal/mcp/server_test.go |
| Context | Agent memory | — | — | GET /v1/memory/{agent}, GET/PUT /v1/memory/{agent}/{name} | memory list, memory push, memory pull | memory_get, memory_put, memory_list | bearer; MCP stdio/HTTP | memory rows | Overwrite by name; push/pull reflect a directory | internal/mcp/server_test.go |
| Attachments | List attachments and mint a private URL | — | — | GET /v1/attachments, GET /v1/attachments/{sha} | attach list, attach get, attach usage | attachment_list, attachment_get_url | bearer; MCP stdio/HTTP | Attachment metadata | Immutable by design | internal/mcp/server_test.go, internal/attachments/attachments_test.go |
| Attachments | Upload an attachment | — | — | PUT /v1/attachments | attach put | attachment_put_from_path (stdio only) | bearer; local stdio MCP | Attachment row | Immutable by design; the same bytes return the same record | internal/mcp/server_test.go |
| Projects | Read projects and their owner | — | GET /ui/api/board/projects, GET /ui/api/board/lanes | GET /v1/tasks/projects, GET /v1/tasks/projects/owners, GET /v1/tasks/projects/{project}/owner, GET /v1/projects/{name}/frontier | tasks projects, tasks owner, tasks owners | projects_list | Operator, bearer, MCP bearer/Access | None (read) | n/a | internal/api/project_owner_test.go, internal/mcp/meta_test.go |
| Projects | Create a project / edit project metadata | — | — | POST /v1/tasks/projects, PUT/PATCH /v1/tasks/projects/{project}/metadata | tasks projects add, tasks project-status | — | bearer | project row; task_events for a task move | Set again | cmd/handoffkeep/project_test.go, internal/api/task_project_test.go |
| Projects | Set the design-lane owner | — | — | PUT /v1/tasks/projects/{project}/owner | tasks owner --set, tasks owner --clear | — | bearer | project owner row; note | Set again or clear | internal/api/project_owner_test.go, cmd/handoffkeep/project_owner_test.go |
| Projects | Milestones | — | — | POST /v1/milestones, PUT/PATCH /v1/milestones/{milestone}, GET /v1/milestones/{milestone}, POST /v1/tasks/{id}/milestone | tasks milestone | — | bearer | milestone row; task_events for the task link | Update or clear (--clear) | cmd/handoffkeep/task_model_test.go |
| Projects | Areas | — | — | GET /v1/task-areas, POST /v1/tasks/{id}/area | tasks areas, tasks set-area | — | bearer | task_events field=area | Set again | internal/api/task_area_test.go |
| Views | Saved searches and views | — | — | — | — | — | — | — | — | — |
| Deploys | Read deploy records and pending risk | — | GET /ui/api/deploy-pending | GET /v1/deploy-services, GET /v1/services, GET /v1/services/{name}/pending | services records-list, services list, services pending | — | Operator, bearer | None (read) | n/a | internal/ui/dashboard_test.go, internal/api/deploys_test.go |
| Deploys | Start / finish a deploy attempt | — | — | POST /v1/deploys, POST /v1/deploys/{attempt_id}/finish | deploys start, deploys finish | — | bearer | deploy attempt row | Finish with rolled_back; a new attempt is a new record | internal/api/deploys_test.go, cmd/handoffkeep/deploys_test.go |
| Deploys | Configure deploy services | — | — | PUT /v1/deploy-services/{name}, POST /v1/deploy-services/{name}/deactivate, PUT /v1/services/{name} | services records-put, services records-deactivate, services put | — | bearer | service row | Configure again; deactivate is not a delete | internal/api/deploy_services_test.go, cmd/handoffkeep/deploys_test.go |
| Translations | Read Korean translations | YouTrack owns the Korean summary and displays it | Rendered title/body/question/options everywhere | GET /v1/translations/missing | translations list | task_show (title_ko), decisions_pending (question_ko, options_ko) | Operator, bearer, MCP bearer/Access | Translation row (source hash, translator, stale flag) | n/a | internal/api/translations_test.go, internal/mcp/operator_test.go |
| Translations | Write Korean translations | The YouTrack summary is the operator's edit path for the displayed title | — | PUT /v1/translations | translations put, translations import | — | bearer | Translation row (translator, source hash) | Overwrite; a changed original marks the row stale | internal/api/translations_test.go, cmd/handoffkeep/translations_test.go |
| YouTrack | Run sync and read its health | The sync itself; cursor, outbox and processed activities are hk bookkeeping | Board detail shows the cached priority/lane with a stale flag | — | youtrack sync | sync_status | bearer (CLI); MCP bearer/Access | cursor, outbox, processed activities, field cache | Rerun; pending outbox is retried with backoff | internal/youtrack/sync_test.go, internal/mcp/meta_test.go |
| YouTrack | Map hk projects to YouTrack projects | `youtrack_projects` is the mapping | — | — | youtrack projects list, youtrack projects put | — | bearer (CLI) | youtrack_projects row | put again | internal/youtrack/client_test.go, cmd/handoffkeep/youtrack_test.go |
| Fleet | Set a host's accepting flag | — | POST /ui/api/nodes/{machine}/accepting | — (the hub route is /v1/nodes/{machine}/accepting on the hub, not hk) | — | — | Operator, or a service identity with JSON content type | ui.write | Set again | internal/ui/routes_test.go |
| Relay | Compose one lane event | — | POST /ui/compose | POST /v1/relay/events | — | — | Operator (console); bearer (API) | ui.write; relay event row | A new event is a new record; delivery is tracked separately | internal/ui/write_test.go, internal/ui/routes_test.go |
| Meta | Describe the task state machine | — | State labels and transitions are rendered in the console | — | — | task_states | MCP bearer/Access; local stdio | None (read) | n/a | internal/mcp/meta_test.go |

## MCP follow-up list

Every row above whose MCP column is **—** is collected here. Two kinds exist:
planned gaps (a follow-up task) and deliberate exclusions (no task, with the
reason). A new feature PR moves its row out of this list or adds a reasoned
exception to `ConsoleWrites`.

| Capability | Why MCP has no tool | Follow-up |
| --- | --- | --- |
| State transition | State/lane writes need the expected-version and idempotency contract | hk 3465 |
| Priority | Same write contract; today only a triage exit sets it through the API | hk 3465 |
| Lane / relane | Same write contract | hk 3465 |
| Project (set on a task) | Basic task write | hk 3465 |
| Labels | Basic task write | hk 3465 |
| Due date | Basic task write | hk 3465 |
| Subscribers | Basic task write | hk 3465 |
| Parent / children | Work-management write | hk 3474 |
| Relations | Work-management write | hk 3474 |
| Split / move | Move is relane on the API, console and CLI; split is not implemented on any surface. The MCP write contract is pending. | hk 3474 |
| Ask a decision | Asking is an agent/CLI producer path; dots's own requests go through the dots request flow (hk 3464) | not planned |
| Deny a pending confirmation | Only the verified operator path produces the attempt being denied | not planned |
| Snooze a decision record | Part of the attention inbox | hk 3474 |
| Legacy answer (task/escalation/lane) | Superseded by structured requests, which `decision_answer` serves | not planned |
| Attention inbox read | Attention inbox over MCP | hk 3474 |
| Inbox state write | Attention inbox over MCP | hk 3474 |
| Inbox undo | Attention inbox over MCP | hk 3474 |
| Disposition answer | Attention inbox and disposition work | hk 3474 |
| Disposition batch accept | Bulk changes with per-item results | hk 3474 |
| Disposition renotify | Attention inbox | hk 3474 |
| Edit a filed title or body | No surface edits task prose after filing; a design decision is needed before a tool | unscheduled |
| Project write (create/metadata) | Work-management write | hk 3474 |
| Project owner write | Work-management write | hk 3474 |
| Milestones | Work-management write | hk 3474 |
| Areas | Work-management write | hk 3474 |
| Saved searches and views | No surface has it yet | hk 3474 |
| Deploy records read | Read-only deploy surface for dots | unscheduled |
| Deploy start/finish | High-impact action; the design requires an approval record, not only a tool | not planned |
| Deploy service config | High-impact action, operator/agent administration | not planned |
| Translation write | Korean translation authoring is a CLI/translation-job surface | unscheduled |
| YouTrack project mapping | Operator administration of the mirror | not planned |

## MCP tools at the baseline

All 22 tool definitions at 64942ff. "HTTP" means `/mcp` streamable HTTP; "stdio"
means `handoffkeep mcp`. The operator and meta tools are registered only for HTTP,
because that path has an authenticated principal.

| Tool | Kind | Transport | Capability row |
| --- | --- | --- | --- |
| search_context | read | HTTP, stdio | Context search |
| recent_checkpoints | read | HTTP, stdio | Checkpoints |
| put_checkpoint | write | HTTP, stdio | Checkpoints |
| get_document | read | HTTP, stdio | Documents read |
| put_document | write | HTTP, stdio | Documents write |
| list_documents | read | HTTP, stdio | Documents read |
| memory_get | read | HTTP, stdio | Agent memory |
| memory_put | write | HTTP, stdio | Agent memory |
| memory_list | read | HTTP, stdio | Agent memory |
| attachment_list | read | HTTP, stdio | Attachments read |
| attachment_get_url | read | HTTP, stdio | Attachments read |
| attachment_put_from_path | write | stdio only | Attachment upload |
| task_search | read | HTTP | Tasks read |
| task_show | read | HTTP | Tasks read, comments read |
| task_list | read | HTTP | Tasks read |
| decisions_pending | read | HTTP | Decisions list |
| decision_answer | write | HTTP | Decisions answer |
| task_comment | write | HTTP | Comment write |
| task_intake | write | HTTP | Task create |
| projects_list | read | HTTP | Projects read |
| task_states | read | HTTP | State machine |
| sync_status | read | HTTP | YouTrack sync health |

Read tools set `readOnlyHint: true`. The write tools (`put_checkpoint`,
`put_document`, `memory_put`, `attachment_put_from_path`, `decision_answer`,
`task_comment`, `task_intake`) set `readOnlyHint: false` and
`destructiveHint: false`. Of these, only `decision_answer` can move a task's
state, by answering the task's request; the rest add records.
