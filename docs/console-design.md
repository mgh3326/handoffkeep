# Console design

`/ui` is one shell around four navigation pages — 홈 · 결정 · 작업 · 배포 — plus
the task record and document deep links. This page describes the system that
ships, not a proposal. Palette derivation, the component specification and the
pre-delivery checklist live in the hk doc
`design/2026-11-06/console-design-system`; the checklist is answered for the
whole console in the change that updates this page. Captures with fictional data
are in [console-shots](console-shots/capture.json).

## Superseded

Earlier revisions of this page described the #117 adoption and a set of static
mocks. Those are superseded: #164 (hk 3140) replaced the old pages with the
shared shell, and #198–#202 (D1–D5) shipped the shell and page redesign covered
here.

The mock pages, **My work**, the command palette and the Fleet hosts, Grades and
docs-browse screens no longer have a view. `/ui/fleet`, `/ui/grades` and
`/ui/docs` render the moved notice, and the `/ui/queue` bookmark redirects to
Work. The server-rendered `/ui/timeline`, `/ui/compose` and
`/ui/decisions/legacy` pages remain outside the new navigation.

## Tokens

The system is **Dense Dark Minimal**. Dark is the default; light is a derived
palette on the same roles. Every value a page uses is a `var(--hk-*)` token from
[`design/tokens.css`](../web/console/src/design/tokens.css) — no per-page hex,
no second stylesheet.

| Role | Tokens |
| --- | --- |
| Surfaces | `--hk-surface-0`, `--hk-surface-1`, `--hk-surface-2`, `--hk-surface-selected` |
| Lines and ink | `--hk-border-subtle`, `--hk-border-control`, `--hk-text-primary`, `--hk-text-secondary`, `--hk-text-muted`, `--hk-text-heading` |
| Action | `--hk-action-bg`, `--hk-action-fg` (lime; one action color) |
| State ink | `--hk-success`, `--hk-warning`, `--hk-danger`, `--hk-info`, `--hk-state-developing` (one per state) |
| Chips | `--hk-chip-decision-bg`, `--hk-chip-decision-fg` (one pair per group), `--hk-chip-mix`, `--hk-chip-base` |
| Focus and motion | `--hk-focus`, `--hk-motion-fast`, `--hk-motion-expand` |

Chip fills are the group ink mixed into the surface (`color-mix`), so the text
keeps its contrast in both themes. Type is `--hk-type-page`, `--hk-type-section`,
`--hk-type-row`, `--hk-type-meta` and `--hk-type-mono` with the matching
`--hk-line-page`, `--hk-line-section`, `--hk-line-row`, `--hk-line-meta` and
`--hk-line-mono`; 12px is the smallest size. Dense rows use `--hk-row-dense` (40px);
touch targets are at least `--hk-touch-height` (44px). Radii are
`--hk-radius-chip`, `--hk-radius-control`, `--hk-radius-card` and
`--hk-radius-pill`. Motion is background/color at `--hk-motion-fast` and
expanders at `--hk-motion-expand`; `prefers-reduced-motion: reduce` sets both to
0ms.

Fonts are self-hosted under `/ui/static/console/fonts` and loaded from
`design/fonts.css`: Pretendard Variable for UI text (dynamic subsets with
`unicode-range`, `font-display: swap`) and JetBrains Mono for ids, SHAs and
digests. There is no remote font dependency.

The base layout breakpoint is 768px: below it the navigation is a fixed bottom
tab bar and rows step up to 16/22; from 768px the navigation is a 216px sticky
sidebar. Detail panes appear from 1024px. Focus is a 2px `--hk-focus` outline
with a 2px offset, inset inside lists so it is not clipped.

## Shared components

| Component | File | Behaviour |
| --- | --- | --- |
| `Icon`, `IconFrame`, `IconButton` | `shared/Icon.tsx` | One 24-unit, 1.75-stroke icon set. Decorative icons are `aria-hidden`; a standalone icon takes `label`, and `IconButton` requires one. |
| `StateIcon` | `shared/StateIcon.tsx` | The state shapes, coloured by the `--hk-state-developing`-style role through `currentColor`. Every state has a distinct outline, and `srLabel` exposes the Korean name. |
| `Chip` | `shared/ui/Chip.tsx` | Static `<span>` with `status`, `priority`, `lane` and `count` variants. The status variant's group is `decision`, `input`, `working`, `failed`, `done` or `neutral`, decided by `statusGroup` in `shared/states.ts`. A lane over 16 characters is cut with the full name in the accessible name. |
| `Mono`, `MonoRef` | `shared/ui/Mono.tsx`, `shared/ui/MonoRef.tsx` | Mono ids and refs; `MonoRef` shows a shortened SHA or digest with a copy button, announcing through the shell's one live region. |
| `CollapsedWarning` | `shared/ui/CollapsedWarning.tsx` | A native `<details>`, collapsed by default: warning icon, count, 펼치기/접기, then the body. |

`statusGroup` decides the chip group of a task; the decision screens map their
own record states in `decisions/DecisionChip.tsx`. `STATE_LABEL`,
`STATE_GROUP_ORDER` and `CHIP_GROUP_ORDER` hold the Korean labels and order.
Reads go through `getJSON`/`HttpError` (`shared/http.ts`) and `usePoll`
(`shared/usePoll.ts`); a failed poll keeps the last good data and marks it, and
loading/failure/staleness are always visible with `role="status"`/`role="alert"`.

## Shell and navigation

`Shell` (`app/Shell.tsx`) draws the brand, the theme button, one `<nav>` and the
page. Its `destinations` table is the navigation: 홈 (`/ui`), 결정
(`/ui/decisions`), 작업 (`/ui/work`), 배포 (`/ui/deploys`) — the decision is
second because a pending decision outranks everything else. A new page is one
row in that table.

The decision entry shows a count chip and an overdue dot, with the spoken form
"열린 결정 N건, 기한 경과 M건" in a polite live region; the shell polls
`/ui/api/attention` every 60 seconds. The theme button reads 밝은 테마 / 어두운
테마 with a sun or moon icon, and `bootTheme` applies the stored choice before
the first render.

## Home — `/ui`, `/ui/home`

The inbox answers "what is waiting for me". A summary line counts decisions
(with overdue), tasks and pending deploys. **지금 할 일** merges decisions and
stalled or decision-blocked tasks, overdue first and then newest; **참고** lists
merged-but-not-deployed work. Rows are grouped 기한 지남 / 오늘 / 어제 / 이번 주 /
지난주 / 이전.

Row anatomy: state icon (or the warning icon for a stalled task), `#id` in
`Mono`, the title, the age, a one-line status word (결정 대기, 멈춘 작업,
머지 후 미배포) and its chips — source (요청/알림), overdue or remaining time,
lane. Unseen rows carry a dot and seen rows are dimmed; a truncated list says
`+N개 더 있음`. The empty state is "대기 중인 일 없음", a failed first load is a
`role="alert"`, and a stale list keeps the last data with a note.

## Work — `/ui/work`

The work list follows execution. The header holds the project `select`; the
tabs are 내 차례 (default), 진행 중, 대기, 완료 and 전체, each with a live count.
Filters (검색, 레인, 종류, 상태, 우선순위 ≥) sit in a `<details>` sheet below
1024px and are inline above it; active filters can be removed one by one. The
URL carries `project`, `view` and the filters, and `/ui/queue` redirects here.

Rows are grouped by chip group in the fixed order 결정 필요 · 분류 대기 · 진행 중 ·
실패 · 완료 · 대기, then sorted by priority, `updated_at` and id. Row anatomy:
`StateIcon`, `#id`, title, then meta chips — status, the active job
(`role · model · profile · host`), lane, priority (warning at P1) and age. From
1024px a selected row opens the task record in a side pane; ArrowUp/ArrowDown
move focus between rows.

## Decisions — `/ui/decisions`, `/ui/decisions/{id}`

The list has four tabs — 열림, 기한 경과, 답변 기록, 정리 필요 — with counts on
the first two, and a 모두 확인 button that records what was seen. Tabs stay on one
line; on a phone the strip scrolls sideways with a fading edge. **새 항목**
divides unseen rows from the rest. Each row carries a state icon, `#id`, the
summarised question, and chips for source, record state, remaining deadline,
lane, new revision and age. Each tab has its own empty state with a
link to the next useful tab; from 1024px a selected row opens the decision in a
side panel.

The decision page shows the record state chip, `결정 #id`, the project/lane and
task link, the request id, revision and answer policy, then the full question.
Options are large `role="radio"` cards with a numbered label, a 권장 chip on the
recommended option and a 미응답 시 기본값 chip on the default; a free-text memo
appears when the request allows it. A deadline/default-action box, 맥락 and 이력
disclosures, and the 답변 확인 call to action follow. Confirming opens
`ConfirmSheet`; the result is a `ResultBadge`, and a failed or unconfirmed send
keeps the selection and its error.

## Deploys — `/ui/deploys`

One card per service, sorted by service name. The header is the service, the
target chip and the repo in `Mono`. The status line names 현재 판 with a shortened
ref in `MonoRef`, then 마지막 시도 with the result chip and its age (성공, 실패,
롤백). 최근 시도 shows three attempts with 더 보기; each attempt is result,
version, record key, time and failed step, plus a warning when the deploy
command finished but the serving version may have changed. A missing time reads
시각 미기록, never a guess.

A malformed-record warning is a `CollapsedWarning`: `N개 기록 형식 불일치 ·
펼치기`, with counts by reason and the failing keys inside. 마지막 배포 이후
머지됨 lists each merged task with its PR and age; when the deploy time is
missing, the list explains why it cannot be computed. A service with no records
reads 기록 없음, and a capped scan says so instead of claiming the fleet truth.

## Operator commands

Actions that touch something outside hk are operator commands, not direct
writes. The console records the intent (`POST /ui/api/tasks/{id}/commands`,
the same Access, origin, CSRF and idempotency guards as every operator write),
hk relays exactly one `lane.event` to the owner of the target, and the
executor reports a receipt (`POST /v1/commands/{id}/receipt`). The console
shows the ladder 기록됨 → 전달됨 → 접수됨 → 완료 | 실패 | 만료 and never shows
done before the receipt. A row with a command in flight carries a `지시` chip in
its meta line; the task record's 운영 명령 section shows every command's ladder
with its times, and a failure shows its error code in Korean.

The tracer kind is `worker.message` (`{text, intent}` with `intent` `steer` or
`queue`), offered on a task with an active job as 워커에게 지시: 지금 끼어들기
records a steer, 현재 작업 끝나고 records a queue. `job.stop`, `task.respawn`
and `deploy.approve` are in the registry but disabled with 실행기 준비 전 until
their executors land. `worker.message` expires after 30 minutes.

The relay body is the documented executor contract:

```text
[hk 명령 #<id> · 지금 끼어들기] <text>
[hk 명령 #<id> · 현재 턴 뒤] <text>
```

`event_id` is the command key. An executor that can queue holds a
`현재 턴 뒤` instruction until the worker's current turn ends; one that cannot
queue treats it as a steer and reports `queue_unsupported` in its receipt.
Receipts are `acked`, `done` or `failed` (with `error_code` and an optional
`note`); they move forward only and a repeated receipt is a no-op. A failure is
legal straight from 기록됨, so an executor that cannot deliver can say so
before the relay is confirmed. When a command is superseded or expires before
dispatch, its undelivered relay row is closed (`delivered_to`
`cancelled:superseded` / `cancelled:expired`), so the next drain skips it. A
drain that already fetched the row can still deliver it; that in-flight
delivery is recorded as a late delivery instead of being hidden, and a delivery
confirmed after the deadline marks the command 만료 with a `만료 후 전달` note.
A receipt that arrives after the ladder closed is kept as a late report in the
timeline (`대체 후 보고` / `만료 후 보고`) and answers 200 rather than being
refused, and the authenticated API client is recorded as the receipt author.
Executors poll `GET /v1/commands?executor_lane=<lane>&state=acked` or the CLI
`handoffkeep commands list|receipt`.

## Task record and document viewer

`/ui/tasks/{id}` renders `TaskRecord`: Closure, Origins, Documents, Changes,
Jobs, Durations, Operator commands and Events. Work rows, Home rows and
decision pages link here.

`/ui/doc/{key}` renders `DocView`: the key, kind and update time, then the
markdown through a lazy renderer; a payload that cannot be rendered is shown as
raw text rather than an empty document. The document is the only view that loads
the markdown chunk, so Home and Work never pay for it.

## Captures

Nine routes — home, work, task, task commands, instruct dialog, decisions,
decision, deploys and document — at 1440px and 390px in dark and light, 36
PNGs. Each page is one viewport as tall
as the document, so the fixed tab bar sits at the real bottom edge instead of
being stitched into the middle by a full-page capture. The filenames encode
page, theme and width; the [manifest](console-shots/capture.json) records the
browser, URL, overflow check and page errors for every view.

Regenerate with the pinned Node runtime and an isolated `playwright-core`
install:

```sh
HK_CONSOLE_CAPTURE_DIR=/tmp/hk-console-pages go test -p 2 ./internal/ui -run TestConsoleRouteDocuments
NODE_PATH=/tmp/hk-console-capture/node_modules node docs/console-capture.cjs
```

Set `CHROME_PATH` when Chrome is installed elsewhere. The harness serves the
embedded shell document and the committed bundle against fictional same-origin
responses; it starts no server and closes the Chrome process it launches. A
`/ui/static/*` request is read from `internal/ui/static`, so fonts load from the
same bundle as production.
