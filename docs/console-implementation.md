# Console implementation

Every existing HTML console route now uses the shared workspace shell and canonical tokens. `/ui/events` remains the SSE transport used by Timeline and Decisions. No deployment is included.

| Old outer template / route | Replacement |
| --- | --- |
| `board.html`: `/ui/queue`, `/ui/tasks/{id}` | `board.html`, ConsoleBoard, shared TaskPage/detail renderer |
| `dashboard.html`: `/ui/home` | `console.html`, Inbox attention queue and DashboardApp |
| `fleet.html`: `/ui/fleet` | `fleet.html`, FleetApp with shared quota pools |
| `docs.html`: `/ui/docs` | `console.html`, DocsApp and document reader |
| `grades.html`: `/ui/grades` | `console.html`, catalog under Fleet hosts; mobile rows |
| `deploys.html`: `/ui/deploys` | `deploys.html`, DeploysApp and recorded-deployment comparison |
| `ui.html`: `/ui/decisions`, `/ui/timeline`, `/ui/compose`, `/ui/doc/{key}` | `server.html` content mounted inside the same React shell |
| `/ui` | Redirects to Inbox at `/ui/home` |

Existing form and fragment URLs remain compatible. The board, fleet, and deploy entry-template filenames are retained for the artifact contract, with the new shell markup. Dashboard, document, and grade mounts share `console.html`. Shared production shell/search/state modules live in `src/shared`, dashboard components live in `src/dashboard`, and fixture/mock sources stay isolated. The old queue prototype is retained for its isolated preview and existing behavior tests; the production entry serves ConsoleBoard. Vite replaces the committed bundle in one build. The duplicate proposal token file and old mock screenshots have been removed.

The board exhausts cursor pages up to its existing 5,000-row safety cap and labels partial counts. State, lane, claimant and title filters are recorded in the URL. My work uses an explicit operator-selected scope. The same-origin board API now batches the #114 durable work-board, immutable placement, and terminal-job reads. It supplies recorded host, canonical role, model, family, PR, and all active sessions while retaining the primary lead after completion. Live job observations stay separate from recorded placement, including when their hosts differ. Missing fields remain explicit. All child sessions are available through the sessions disclosure. PR chips link only validated HTTPS GitHub pull URLs.

Task detail retains body documents, comments, request history, task events, and benchmark coverage. The properties panel follows content on mobile. Report paths remain text with “Report unavailable in console”; document links use the document API.

Inbox labels its task/lane decision count as partial and explicitly excludes answerable escalations, linking to Decisions for the full queue.

Decisions retain server aggregation and the existing write gates. Confirmation happens in-page. A failed or timed-out write retains the form and its selection, and requires checking history before retrying. Stream updates do not replace an edited form. Structured request records remain read-only because the current server deliberately does not provide their answer transport; legacy request and disposition forms use their existing endpoints. Resolved structured history is available on task detail.

The adapter works with Linear delivery disabled, preserves projected PRs and sessions after placement retention, and does not write projection/outbox rows. Its allowlist excludes account keys, candidate telemetry, pane identifiers, and verdict text. Fleet power is unavailable and load divided by cores is described as a load ratio. Account quotas remain separate shared pools with their recorded time/reset/expiration semantics. Document search remains browsing/filtering; the palette searches tasks without claiming document full-text search.

Both themes use the approved palette with derived light values. The light information ink and control borders are adjusted for contrast. Token checks cover 4.5:1 text/state ink on every surface, 3:1 focus/control borders, and the primary action pair.

## Browser captures

The harness uses actual embedded Go route documents and committed production JS/CSS with fictional responses, without a database or local server. It checks page errors, horizontal overflow, desktop sidebar height, row activation, task return focus, palette opening, and explicit answer confirmation/failure retention. It closes the Chrome process it starts.

Regenerate with the pinned Node runtime and an isolated `playwright-core@1.56.1` install:

```sh
HK_CONSOLE_CAPTURE_DIR=/tmp/hk-console-pages go test -p 2 ./internal/ui -run TestConsoleRouteDocuments
NODE_PATH=/tmp/hk-console-capture/node_modules node docs/console-capture.cjs
```

| View | Dark 1280 | Light 1280 | Dark 390 | Light 390 |
| --- | --- | --- | --- | --- |
| inbox | [PNG](console-shots/inbox-dark-1280.png) | [PNG](console-shots/inbox-light-1280.png) | [PNG](console-shots/inbox-dark-390.png) | [PNG](console-shots/inbox-light-390.png) |
| board | [PNG](console-shots/board-dark-1280.png) | [PNG](console-shots/board-light-1280.png) | [PNG](console-shots/board-dark-390.png) | [PNG](console-shots/board-light-390.png) |
| task | [PNG](console-shots/task-dark-1280.png) | [PNG](console-shots/task-light-1280.png) | [PNG](console-shots/task-dark-390.png) | [PNG](console-shots/task-light-390.png) |
| decisions | [PNG](console-shots/decisions-dark-1280.png) | [PNG](console-shots/decisions-light-1280.png) | [PNG](console-shots/decisions-dark-390.png) | [PNG](console-shots/decisions-light-390.png) |
| deploys | [PNG](console-shots/deploys-dark-1280.png) | [PNG](console-shots/deploys-light-1280.png) | [PNG](console-shots/deploys-dark-390.png) | [PNG](console-shots/deploys-light-390.png) |
| fleet | [PNG](console-shots/fleet-dark-1280.png) | [PNG](console-shots/fleet-light-1280.png) | [PNG](console-shots/fleet-dark-390.png) | [PNG](console-shots/fleet-light-390.png) |
| docs | [PNG](console-shots/docs-dark-1280.png) | [PNG](console-shots/docs-light-1280.png) | [PNG](console-shots/docs-dark-390.png) | [PNG](console-shots/docs-light-390.png) |
| grades | [PNG](console-shots/grades-dark-1280.png) | [PNG](console-shots/grades-light-1280.png) | [PNG](console-shots/grades-dark-390.png) | [PNG](console-shots/grades-light-390.png) |
| timeline | [PNG](console-shots/timeline-dark-1280.png) | [PNG](console-shots/timeline-light-1280.png) | [PNG](console-shots/timeline-dark-390.png) | [PNG](console-shots/timeline-light-390.png) |
| compose | [PNG](console-shots/compose-dark-1280.png) | [PNG](console-shots/compose-light-1280.png) | [PNG](console-shots/compose-dark-390.png) | [PNG](console-shots/compose-light-390.png) |
| document | [PNG](console-shots/document-dark-1280.png) | [PNG](console-shots/document-light-1280.png) | [PNG](console-shots/document-dark-390.png) | [PNG](console-shots/document-light-390.png) |
| palette | [PNG](console-shots/palette-dark-1280.png) | [PNG](console-shots/palette-light-1280.png) | [PNG](console-shots/palette-dark-390.png) | [PNG](console-shots/palette-light-390.png) |

[Capture manifest](console-shots/capture.json) records the browser and all 48 results.

To refresh only changed board and palette views while preserving the other capture records, set `HK_CONSOLE_CAPTURE_VIEWS=board,palette` when running the harness.
