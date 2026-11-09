# YouTrack structure for the handoffkeep import

This proposal makes YouTrack the place the operator opens first, while keeping hk's stable identifiers visible so the migration can be checked and repeated. It follows `decision/2026-11-09/youtrack-canonical-r8` (hk:doc): tasks, comments, documents and decisions move into YouTrack; hk's transactional job, lease, event, outbox, idempotency and ID mapping records remain in PostgreSQL until their later slices are designed.

## Projects

Create one YouTrack project for each value returned by `handoffkeep tasks projects`: `ParkingArcade`, `admiral`, `agent-skills`, `auto_trader`, `auto_trader-operator`, `brewdial`, `cordglean`, `experiment`, `fillwire`, `fleet-ops`, `handoffkeep`, `herdr`, `mergewake`, `other`, `pacelever`, `paneglance`, `panewire`, `peach-berry-studio`, `plane-pilot`, `recallbeam`, `remy-client`, `robin-prefect-automations`, `rstack`, `scopefuel`, `shiftgate`, `wrk` (read from the current CLI on 2026-11-09). Keep the project name equal to the hk project; generate a short YouTrack key and retain the exact source name in `hk 프로젝트`.

Add two explicit buckets: `HK` for legacy tasks with a null project and `Shared` for documents not attached to a task. Use `DEC` for decision records. `DEC` is separate from the task-project mapping already used by `internal/youtrack/schema.go` (`DecisionProject = "DEC"`). A decision issue carries both its decision record ID and, when present, its linked task ID.

## Issue fields

The current Go sync contract in `internal/youtrack/schema.go` and `internal/youtrack/sync.go` supplies the Korean field labels below. Keep these names stable; create per-project fields and value sets from the imported rows.

| Field | Type / values | Source |
|---|---|---|
| `hk ID` | integer, required for each imported task and decision | task ID or decision record ID |
| `hk 프로젝트` | string | task project, including explicit `Unassigned` for legacy tasks |
| `hk 상태` | state bundle: 분류 대기, 대기, 착수 전, 조사 중, 개발 중, 검토 중, 머지됨, 배포됨, 완료됨, 종료됨, 중복, 보류(보관) | the twelve `StateLabels` in `internal/youtrack/schema.go` |
| `hk 우선순위` | integer | task priority |
| `레인` | enum, values discovered from task rows | lane |
| `종류` | enum, values discovered from task rows | task kind |
| `등급` | enum (`S+`, `S`, `A+`, `A`, `B`, `C`, plus observed values) | title marker `(grade X, Tn)`, then `refs.linear.grade`, as `internal/youtrack/sync.go` does |
| `티어` | enum (`T0`–`T3`, plus observed values) | title marker, then `refs.linear.tier` |
| `PR` | string | `refs.pr` |
| `작업 job` | string | `refs.job_id` |
| `멈춤` | enum with `멈춤` | `stalled=true` |
| `hk 문서 키`, `문서 prefix` | string, enum | document key and its first path component on articles |

`DEC` also has `결정 ID` (integer), `결정 상태` (열림, 답변됨, 대체됨, 만료됨), `권장` (string), `결정 기한` (date), and `연결 task ID` (integer). Carry the original question in the issue summary and the answer/recommendation/default in its description. Do not use a decision's linked task ID as its `hk ID`.

## Tags and navigation

Keep typed values in custom fields for filtering and use a small tag vocabulary for fast visual scanning: `hk/stalled`, `hk/awaiting-decision`, `hk/decision`, and `hk/project/<name>`; `hk/lane/<name>`, `hk/kind/<name>`, `hk/grade/<grade>`, and `hk/tier/<tier>` are useful when scanning across projects. The importer attaches these tags from the corresponding source fields.

The first screen should be a small operator dashboard (created after the import): **Active work**, **Decision inbox**, and **Knowledge base**. Active work links to a global saved search for non-terminal tasks and a board with columns from `hk 상태` and swimlanes from `레인`. Add one saved search per lane and one per project; add project-filtered boards only for projects with enough active work to benefit. Keep `DEC`'s decision inbox prominent and separate from execution flow.

The custom-import API does not import Agile boards, reports, or dashboards (JetBrains, [Custom Imports](https://www.jetbrains.com/help/youtrack/cloud/2026.2/custom-imports.html), imported-entity list). Create the searches, boards, and dashboard with YouTrack REST/Hub REST after the test-project spike, or configure the small first screen manually. Do not claim that the import script creates them.

## 技술 자료 tree

Use each YouTrack project as the first level. Inside it, show article titles as `[<first key component>] <full hk key> [<kind>]`. Keep `hk 문서 키` as the exact unique key and `문서 prefix` as its first component, so saved searches can form a useful second level without changing the source identifier. Put unattached documents in `Shared`; task-linked documents follow that task's project, including `HK` for an unassigned task. This preserves one copy of each document and allows filters such as `문서 prefix = design` within a project.

Documents over the importer's conservative 240 KiB part target get an index article and ordered `part-0001`, `part-0002`, … child articles. Every part retains the original document's `hk ID`, `hk 문서 키`, author and timestamps; the index points to each part. The spike must establish YouTrack's actual article limit and confirm that part articles preserve their parent link. Text content is imported in full. Binary and large artifacts (screenshots, pane captures, logs, benchmark data, other job artifacts) stay in Google Drive or R2 and remain linked from document text; never upload them as YouTrack attachments.

## Source evidence and limits

- `handoffkeep tasks projects` returned the 26 project names above.
- `handoffkeep tasks list --project=handoffkeep --limit=2` returned ID-ordered task records with `title`, `kind`, `state`, `priority`, `refs`, `created_by`, `created_at`, `updated_at`, `project`, labels and lifecycle fields. The unfiltered CLI sample included old rows with `project: null`; the HK bucket accounts for them.
- `handoffkeep doc list --prefix design/` returned keys such as `design/2026-11-09/youtrack-canonical-r8` and nested keys such as `design/2026-11-09/placement-measure-review/code-findings`, confirming that path components are useful for article grouping.
- `internal/youtrack/sync.go` writes the task state, priority, PR, lane, project, kind, grade, tier and stalled fields. `internal/youtrack/docs.go` currently mirrors a narrower set of document prefixes and caps a single article body at 256 KiB; this initial custom import intentionally includes all document prefixes and parts large text instead of leaving it only in hk.
- The stated import volume is approximately 3.6k tasks, 13k comments and 11k documents / 96 MB. The spike will replace the estimate with measurements.

## Import mode and provenance

The custom import is a one-time snapshot and is safe to re-run by stable external IDs (`hk-task:<id>`, `hk-comment:<id>`, `hk-decision:<id>`, `hk-doc:<id>` and deterministic document-part IDs). Do not enable Continuous import: the export has no reliable updated-since feed, and the decision is to wipe the old Python-mirror content only after the full export, then import the complete snapshot.

The export includes `created_by` / comment author and RFC3339 timestamps. YouTrack import entities accept authors and creation timestamps; the spike verifies what the target instance preserves. Current `DecisionRecord` has `answered_by` but no `created_by`, so the source does not claim to preserve the original decision asker identity. The importer uses that answer author if present, otherwise the lane as available attribution. This gap must be recorded in the spike result.
