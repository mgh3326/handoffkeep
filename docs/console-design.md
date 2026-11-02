# Console design proposal

Implementation status: the console now uses the canonical [tokens](../web/console/src/design/tokens.css) and shared shell. Production-bundle captures with fictional data are in [console-shots](console-shots/capture.json). The proposal below records the approved design and its original review limits.

This proposal makes `/ui` a compact workspace for following tasks, answering
decisions, and checking fleet capacity. The default is a state-grouped task list.
The operator can scan task, host, role, model, and PR without opening each task.

The deliverables are static review artifacts. Existing Go handlers, templates,
React sources, and the committed console bundle are unchanged. The new
[token layer](../web/console/src/design/tokens.css) is not loaded in production.
View implementation follows operator review in a later PR. Nothing is deployed.

## Review screens

Open the HTML files directly from a checkout. They use relative local assets,
system font fallbacks, and fictional data. No build, server, login, remote fonts,
or API connection is needed. Dark is the default. The **Light** button selects
the derived light palette. `?theme=light` selects it on first load.

| Screen | Review focus | Mock |
| --- | --- | --- |
| Work board | State groups, compact rows, host, role, model, and PR chips | [Board](console-mocks/board.html) |
| Task detail | Description, properties, linked PR and report, event timeline | [Task detail](console-mocks/task-detail.html) |
| Decisions inbox | Pending queue, one question, recommendation, explicit answer | [Decisions](console-mocks/decisions.html) |
| Fleet hosts | Load, freshness, unavailable power, separate shared quota | [Fleet hosts](console-mocks/fleet-hosts.html) |
| Command palette | Navigation, tasks, documents, keyboard focus | [Palette overlay](console-mocks/command-palette.html) |

The mocks illustrate one task detail and one decision. Other row links open
those representative fixtures. Inbox and Deploys navigation open related
fixtures because those full screens are outside this brief's mock set.
**My work** demonstrates a fixed fictional assignment. It is not an identity
integration. Search filters the palette's local sample entries. The answer
button previews a choice and never sends an answer.

## Information architecture

Keep the sidebar in this order. A count means work requiring attention, not an
unread count. The workspace name stays fixed above navigation. Operator and
theme preferences stay outside task content.

| Sidebar view | Purpose and route to retain | Existing data source and implementation limits |
| --- | --- | --- |
| Inbox | Start with actionable decisions and work awaiting review. Use the existing `/ui/home` landing route. | `GET /ui/api/dashboard` supplies work and PR groups. `GET /ui/api/glance` supplies decision and disposition counts. Decision requests come from task refs and detail. There is no persisted notification feed, read receipt, or mark-all-read endpoint. This is an attention queue, not a new notification system. |
| My work / Board | Follow execution by state at `/ui/queue`. **All work** is the default. **My work** is an explicit claimant or lane filter. | `GET /ui/api/board/tasks?lane=&state=&parent_lane=&after_id=&limit=` supplies task rows. `GET /ui/api/live` supplies jobs, hosts, and exact task-job links. `claimed_by` and lanes are not verified-email identities. Ask the operator to select a scope rather than infer ownership from email. Placement details need the read adapter described below. |
| Tasks | Search and inspect all lifecycle states using the same `/ui/queue` list and `/ui/tasks/{id}` detail. | `GET /ui/api/search?q=&scope=tasks&limit=` searches tasks and comment snippets. The list and detail APIs supply full task context. Preserve the distinction between search's `has_more` and list pagination. |
| Decisions | Answer pending requests and inspect resolved history at `/ui/decisions`. | `internal/ui/decision_requests.go` already projects request state and history. Task list refs identify current requests. `GET /ui/api/board/tasks/{id}` supplies `decision_requests` and `decision_legacy`. The HTML page also aggregates legacy task and lane decisions, escalations, and dispositions in `ui.go` and `disposition.go`. There is no equivalent aggregate decisions JSON endpoint. Keep that server aggregation or add a same-origin adapter in the implementation PR. |
| Deploys | Compare recorded deployment with subsequent merges at `/ui/deploys`. | `GET /ui/api/deploy-pending` supplies service groups, successful deploy records, later merge events, invalid records, and completeness flags. Say **Merged since last recorded deploy**, not **Definitely undeployed**. This view does not grant deploy authority. |
| Fleet hosts | See accepting, draining, unavailable, and stale hosts at `/ui/fleet`. | `GET /ui/api/live` supplies load, core count, memory, job links, and per-section freshness. `GET /ui/api/fleet` supplies session snapshots. `GET /ui/api/dashboard` supplies quota by account and pool. Host power and host-to-account attribution are unavailable in these BFF responses. |
| Docs / Search | Browse source documents at `/ui/docs` and open global navigation with the palette. | `GET /ui/api/docs/prefixes` and `/ui/api/docs?prefix=&before=&limit=` browse keys and summaries. `GET /ui/api/board/doc?key=` reads one document. Search supports only `scope=tasks`; document browsing can filter loaded titles or prefixes, but must not claim global document full-text search. |

Keep supporting tools discoverable without enlarging the primary sidebar.
The existing benchmark catalog at `/ui/grades`, backed by
`GET /ui/api/bench/catalog`, belongs under Fleet hosts and the palette.
Cleanup candidates use `GET /ui/api/reap`. The existing timeline and event
pages remain accessible from task activity and the palette.

Task detail combines `GET /ui/api/board/tasks/{id}` with paged
`GET /ui/api/board/tasks/{id}/comments?after_id=`. The detail response includes
events, dwell, Linear identity, decision history, and benchmark participants.
Read `body_doc` through the document API. `refs.report_path` is provenance text,
not a browser-readable filesystem URL. Make a report link only when there is
an explicit document key or approved remote link. Otherwise show the path with
**Report unavailable in console**. The mock report is an inline fictional sample.

### Board projection for PR #114

[PR #114](https://github.com/mgh3326/handoffkeep/pull/114) introduces the Linear
work-board projection and immutable placement records. The proposal consumes
that data without making the browser call bearer-token `/v1` routes or Linear.
Sources are the PR's
[work-board guide](https://github.com/mgh3326/handoffkeep/blob/6c4b2f7/docs/linear-work-board.md),
[placement contract](https://github.com/mgh3326/handoffkeep/blob/6c4b2f7/docs/placement-decisions.md),
and [projection types](https://github.com/mgh3326/handoffkeep/blob/6c4b2f7/internal/store/linear_board.go).
These describe the pending dependency, not a deployed console contract.

| Row content | Source today | Proposed use of #114 |
| --- | --- | --- |
| Task, state, priority, project | Board list `id`, `title`, `state`, `priority`, `project`, `area` | Keep hk task state authoritative. Linear's coarser workflow state is secondary. |
| Host | Live `links` joined by exact `refs.job_id`, then jobs' `machine` | Placement `chosen_host` and projected active sessions. Label a recorded placement separately from currently observed execution. Never substitute `decided_on`, which identifies the reporting node. |
| Role | Live job `role` may be normalized or absent. Benchmark participant roles are historical evidence. | Canonical `lead`, `worker`, `review`, `consult` displayed as Builder, Worker, Reviewer, Consultant. Preserve the canonical value in filters and accessible text. |
| Model | Live job supplies tier, not model. Benchmark segments can supply historical `model_id`. | Placement `family` and `model`. Do not infer model from tier or catalog. Missing model reads **Not collected**. |
| PR | Task `refs.pr` and `refs.head_sha` | Keep the PR link beside the execution chips. The snapshot does not establish current GitHub CI or mergeability. |
| Report and events | Task refs, task events, comments, document keys | READY, ESC, BLOCKED, FINDINGS, MERGED, and placement attempts can extend the timeline when a console-safe read projection exists. Do not scrape Linear's managed description. |

The current `/ui/api/board/*` endpoints do not expose #114's complete active
session projection. A future same-origin read adapter must supply that projection
or enrich the board response. Preserve all active sessions per task. Show the
lead session inline with a **+N sessions** chip that opens the rest, rather than
overwrite the lead with the last reviewer. The fixtures show one session per task
to make the row treatment reviewable. Placement history's 180-day retention means
an absent old record is not proof that a task never ran.

Group by the exact hk states from the board response. Labels are Backlog,
Reopened, Claimed, In progress, Verifying, Join, Hold, Needs decision, Merged,
and Dropped. The mock includes five nonempty groups. Keep other nonempty groups
visible, and make empty groups optional. Do not combine Hold and Needs decision.
An open PR is a link, not a reason to overwrite the hk state with Verifying.

Within each group, sort by priority, then task ID as a stable tie-breaker.
Include state and filter parameters in the URL. Restore selected task and scroll
when detail closes. Exhaust the board's `after_id` pagination before showing a
complete count. Until then, show **N loaded**. Display truncation and unknown
fields explicitly.

### Freshness and action semantics

Each live section retains its own `status` and `fetched_at`. Keep last-good data
visible with a stale label after failure. A never-fetched section reads
**Unavailable**, not zero. Unknown load, memory, model, power, and job counts
stay unknown. Load divided by cores is a load ratio, not CPU utilization.

Fleet quota stays in a separate shared-pool section. Do not divide pooled quota
among hosts. Show the measurement time, window, reset, and expired or reset-passed
state. The mock's watts value is unavailable because the current console APIs do
not collect power. #114 candidate power is a historical placement signal, not
current host telemetry.

The decisions screen distinguishes open, overdue, answered, default applied,
withdrawn, superseded, and uncleaned requests. A passed deadline never implies
that a default was applied. Uncleaned terminal-task requests belong in a separate
cleanup group. Legacy unrecorded requests have no invented recommendation.

In the future implementation, use the existing authenticated and CSRF-protected
form routes for answers and comments, including `/ui/decisions/answer`,
`/ui/decisions/answer-batch`, and `/ui/tasks/{id}/comments`. Request display alone
does not imply that its answer transport is available. Respect the existing
write gates, request revision, and hub configuration. Keep a failed answer
editable and visible with its error. Never mark it answered on a timeout.

## Keyboard and focus model

The model is proposed for hk. The reference repository supports the domain and
navigation structure; it does not establish every shortcut below as an observed
Linear binding.

| Key | Behavior |
| --- | --- |
| `j` / `k` or arrows | Move the active row in a list. Do not open it. Keep it visible. Palette uses arrows while its search input accepts text. |
| `Enter` | Open the active task, decision, host, or palette result. For a decision, opening is separate from submitting an answer. |
| `Esc` | Close the topmost palette, menu, or detail. Restore focus to its trigger. Returning from a task restores board selection and scroll. |
| `Cmd+K` / `Ctrl+K` | Open or close the global palette. Focus its search input. Show navigation and recent items before a query. |
| `g`, then `i` | Inbox. |
| `g`, then `b` | Work board. |
| `g`, then `m` | My work with the operator's explicit scope. |
| `g`, then `t` | All tasks. |
| `g`, then `d` | Decisions. |
| `g`, then `e` | Deploys. |
| `g`, then `h` | Fleet hosts. |
| `g`, then `s` | Docs and search. |
| `Tab` / `Shift+Tab` | Native focus order. Trap focus only within the modal palette. |
| `?` | Open a shortcut reference in the implementation. |

Expire the `g` prefix after 900 ms. Ignore letter shortcuts in inputs, textareas,
selects, contenteditable fields, and composition events. Modifiers belong to
their platform shortcuts. Never use a navigation shortcut to submit an answer,
merge, deploy, or toggle host acceptance. Radio options use native arrow and Tab
behavior. Recommend a choice in text without silently submitting it.

The preview implements board `j/k`, native link Enter, palette arrows and Enter,
Escape, the theme toggle, mobile navigation, and the available fixture `g`
destinations. It does not implement production search, `g m`, `?`, persisted
selection, or real writes. The production model above remains implementation
work, not a capability claim for these HTML files.

## Tokens, density, and responsive layout

The source is `hk:doc design/fleet-web/linear-style-tokens`, document revision
`c42825803424592916cf39007d2f96ffad36c848769e41616bec87f8f0dfa250`.
It records a dark marketing-site system. The proposed app layer preserves its
void, carbon, and obsidian surfaces, graphite hairlines, type families, weight
range, spacing scale, radii, and lime primary action.

Semantic `--hk-*` roles isolate components from palette values. The light theme
is derived, not observed or operator-approved. It uses the same roles and a
darker status ink on pale surfaces. Dark review ink and warning amber are also
derived for the app. Original iris and lavender remain available as primitives.
Only one lime primary action appears in an actionable view. The decision mock
uses it for **Preview answer**. Navigation and links stay neutral.

| Concern | Treatment |
| --- | --- |
| Type | Inter Variable, then system sans. Berkeley Mono, then JetBrains Mono, IBM Plex Mono, and system monospace. No remote font dependency. Body 16px, small body 15px, caption and app rows 13px, label 12px, micro 10px. Weights 300, 400, 510, 590. |
| App adaptation | Compact 13px rows and 20–24px page titles replace marketing headings in navigation-heavy views. Exposed 24px and 32px type utilities remain available. |
| Spacing | The supplied 4px base scale through 128px, with named spacing utilities. Desktop list rows are at least 48px. Mobile controls are at least 44px tall. |
| Shape | 2px small, 4px badge, 6px input and button, 12px maximum card radius, pill for circular status marks. |
| Elevation | Surface steps and borders. The palette uses a scrim and strong hairline, without a drop shadow. |
| States | Color plus text or an icon. Disabled ash is not used for essential body text. Null values are words or labeled dashes. Focus uses a visible 2px outline. |

At 1280px the sidebar is 216px, task properties are 248px, and row metadata is
aligned in columns. At 390px the sidebar becomes a menu, task titles wrap, and
every row retains host, role, model, and PR below its title. Detail properties
follow the activity content. The selected decision appears above its question;
other request summaries remain available in a disclosure. Host statistics wrap
under the host name. Quota pools stack. The palette fits within the viewport and
its results scroll independently. No page requires horizontal scrolling.

Use semantic headings, real links and buttons, native radio inputs, text status,
and an inert background beneath the modal. Restore focus when the palette closes.
Do not animate selection or require pointer hover to reveal critical information.
The proposal uses no animation. Font metrics vary across system fallbacks;
captures record the result on local Chrome, not a bundled Inter font.

### Adoption through the existing asset pipeline

The proposal lives at `internal/ui/assets/tokens.css` because the brief names
that review location. The current server embeds `templates/*.html` and `static`
in `internal/ui/ui.go`. It does not embed `assets`. Each mock imports the proposal
through a relative CSS path. No Go route or production stylesheet is changed.

After approval, replace the values in the existing canonical
`web/console/src/design/tokens.css` with the agreed roles and migrate consumers
whose token meanings differ. Build with the pinned Node toolchain and
`npm run build` to emit `internal/ui/static/console/tokens.css`. Commit that
bundle with the source. Go templates and React entries continue to load the same
versioned static stylesheet. Remove the proposal copy after migration so the
app has one token owner. Do not introduce a second framework or a runtime CDN.

Existing reads continue through `getJSON` and validate response shapes.
Polling continues through stable `usePoll` loaders. Malformed responses produce
errors rather than empty lists. A new interaction should reuse board API types
and the current authentication boundary.

## Reference evidence and its limits

The private reference repository was read at commit
`dd876c5665fb628d375f8a98a064089957ec56f4`. Links require repository access.
Only the design conclusions are reproduced here. Private instance data, account
identifiers, hostnames, and captured payloads are not copied into the mocks.

| Reference file | Evidence used and design consequence |
| --- | --- |
| [clients/web/notes/sync-engine.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/clients/web/notes/sync-engine.md) | Bootstrap, partial model loading, IndexedDB collections, subscriptions, and transaction recovery. Keep a stable list while detail loads separately. This is not authorization to copy Linear's sync engine into hk. |
| [clients/web/notes/api-surface.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/clients/web/notes/api-surface.md) | Workflow names differ from state classes, and assignee differs from delegate. Keep task state, responsible claimant, and executing session separate. |
| [clients/web/notes/schema.graphql](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/clients/web/notes/schema.graphql) | Inspected `CustomView`, `Issue`, `IssueHistory`, and `WorkflowState`. Custom views have filters, sharing, sidebar names, and display preferences. The board is a view over tasks, while detail owns history and properties. |
| [extracted/web-idb-schema.json](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/extracted/web-idb-schema.json) | Issue indexes include state, assignee, team, and label dimensions. Comment and history collections link by issue. Keep filters first-class and fetch related detail independently. No IndexedDB work is proposed in this PR. |
| [extracted/web-sw-manifest.txt](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/extracted/web-sw-manifest.txt) | `CustomViewsHeader`, `IssueDetailsPaneSidebar`, `IssueCommentsAndHistory`, Inbox, and shortcut settings name distinct app components. These support sidebar, list, properties, activity, and palette separation. Component names do not establish exact visual geometry or key bindings. |
| [comparison/reusable-patterns.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/comparison/reusable-patterns.md) | Separate responsibility from delegation, defer related detail, and keep field actions bounded. Retain one task timeline across agent activity. |
| [comparison/platform-parity.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/comparison/platform-parity.md) | Shared inbox, issue, view, and my-issues concepts across clients. Keep these recognizable in navigation while adapting to hk's operator workflow. |
| [comparison/cache-strategy.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/comparison/cache-strategy.md) | Distinguishes cached data and pending writes, with acknowledged gaps in older web observations. Never represent a pending or failed answer as accepted. Prefer newer web observations where these notes differ. |
| [comparison/api-auth-models.md](https://github.com/mgh3326/linear-ref-analysis/blob/dd876c5665fb628d375f8a98a064089957ec56f4/comparison/api-auth-models.md) | App credentials and automation clients have different boundaries. Keep console requests same-origin and credentials server-side. Treat the old authentication comparison as historical, not the current deployment contract. |

The 216px sidebar, row geometry, responsive layout, and hk shortcut assignment
are design decisions in this proposal. The supplied analysis does not contain
enough visual evidence to claim a pixel-exact Linear app clone.

## Capture and validation

Each mock has dark and light screenshots at 1280px and 390px under
[`console-shots/`](console-shots/). PNGs use one device pixel per CSS
pixel and capture the whole page, except the palette overlay, which captures
the viewport. The filenames encode page, theme, and width.
The [capture manifest](console-shots/capture.json) records Chrome version,
viewport checks, and the preview interactions exercised.

| Screen | Dark, 1280 | Light, 1280 | Dark, 390 | Light, 390 |
| --- | --- | --- | --- | --- |
| Board | [PNG](console-shots/board-dark-1280.png) | [PNG](console-shots/board-light-1280.png) | [PNG](console-shots/board-dark-390.png) | [PNG](console-shots/board-light-390.png) |
| Task detail | [PNG](console-shots/task-dark-1280.png) | [PNG](console-shots/task-light-1280.png) | [PNG](console-shots/task-dark-390.png) | [PNG](console-shots/task-light-390.png) |
| Decisions | [PNG](console-shots/decisions-dark-1280.png) | [PNG](console-shots/decisions-light-1280.png) | [PNG](console-shots/decisions-dark-390.png) | [PNG](console-shots/decisions-light-390.png) |
| Fleet hosts | [PNG](console-shots/fleet-dark-1280.png) | [PNG](console-shots/fleet-light-1280.png) | [PNG](console-shots/fleet-dark-390.png) | [PNG](console-shots/fleet-light-390.png) |
| Palette | [PNG](console-shots/palette-dark-1280.png) | [PNG](console-shots/palette-light-1280.png) | [PNG](console-shots/palette-dark-390.png) | [PNG](console-shots/palette-light-390.png) |

Capture with a local Chrome installation and an isolated Playwright dependency:

```sh
npm install --prefix /tmp/hk-console-capture --no-audit --no-fund playwright-core@1.56.1
NODE_PATH=/tmp/hk-console-capture/node_modules node docs/console-mocks/capture.cjs
```

Set `CHROME_PATH` when Chrome is installed elsewhere. The capture script opens
`file:` URLs, checks both viewport widths for overflow and script failures,
exercises preview interactions, and closes the Chrome process it launches.
It starts no server.

The brief's focused Go check is `go test -p 2 ./internal/ui`. Database-backed
cases skip locally without `HANDOFFKEEP_TEST_DB_URL`; database integration and
full suites belong in CI under this brief's host-safety policy. No database
server is started. The existing `scripts/check` runs full suites, so it is left
to CI rather than run locally for this static proposal.
