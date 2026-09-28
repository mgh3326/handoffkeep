# Plane vs Linear — one-way mirror comparison (#764)

Comparison of the two external-mirror options on the operator's criteria:
screens, filters, grouping, cost, operating burden, dual-source-of-truth
risk. hk is the sole write source of truth under both options; the
projection is non-terminal tasks only.

## Dry-run numbers (snapshot 2026-09-28, `handoffkeep plane plan`)

Source: read-only `tasks export` of the live queue — 864 tasks.

| Metric | Value |
| --- | --- |
| Tasks that would be mirrored (non-terminal) | **215** (backlog 181, claimed 3, in_progress 6, hold 25) |
| Terminal tasks excluded from projection | 649 (merged 417, dropped 232) |
| Tasks touched last 7 days | 563 |
| Terminal tasks touched last 7 days (archive-churn proxy) | **422/week** |
| Estimated remote calls/day (either option) | ~376 = ~161 writes (2 calls/op at ~80 task-writes/day) + ~215 reconcile reads |
| Sensitive titles withheld | 21 |
| Linear free-tier headroom (250 − 215) | **35 issues** |
| Unmapped projects | 0 with `HK_PLANE_DEFAULT_PROJECT` set; 215 tasks land in the default otherwise |

Notes on the numbers: `touched_last_7d` counts `updated_at` bumps, so it
over-counts transitions (relanes and reclassifies count too) — it is the
conservative upper bound the comparison uses. The calls/day estimate is
dominated by the daily reconcile read pass, not writes.

## Comparison

| Criterion | Plane (hk → Plane, this change) | Linear (hk → Linear, #174 path) |
| --- | --- | --- |
| **Screens** | Kanban board + list per project, inbox, project pages. Human-friendly read surface; no hk data beyond the allowlist ever appears. | Workspace board/list views, strong issue detail page. Terminal tasks persist as archived issues rather than disappearing. |
| **Filters** | Per-project filters on state/label/priority; workspace-level cross-project views exist but are thinner than Linear's. | Workspace-wide saved filters/views across all projects and labels — strongest filter surface of the two. |
| **Grouping** | Project → state/label/priority grouping; hk `project` maps 1:1 to a Plane project, so the board groups exactly like hk. | Group by team/project/label/state; hk project would be a label or Linear project — either loses the 1:1 board-per-project feel. |
| **Cost** | Trial free until ~2026-10-04; self-host CE free beyond that; cloud paid if kept. | Free tier caps at **250 active issues** — current open set is 215 (headroom 35). Terminal tasks must be archived to stay under the cap; recent archive churn is ~422/week, so the cap margin is thin and shrinks as backlog grows. |
| **Operating burden** | New writer: `plane_outbox` drain, daily reconcile, project-map config (`HK_PLANE_PROJECT_MAP`/`HK_PLANE_DEFAULT_PROJECT`), one env-held key. Remove-on-terminal keeps the board self-cleaning. | Connector already built and merged; per-task opt-in via `refs.linear.sync` (not automatic) plus archive/comment flow on terminal. Reconcile CLI exists. Cap accounting is an extra standing concern. |
| **Dual-source-of-truth risk** | Low: projection is allowlist-shaped and structural, drift resolves hk-wins, terminal items are deleted so the board cannot accumulate stale terminal state. Plane item edits are overwritten on next pass. | Low for the same reasons (hk wins, marker dedup). Slightly higher residual: archived terminal issues remain visible/searchable in Linear, and archived items can still be edited remotely without an obvious re-check. |

## Assessment

- **Plane** preserves hk's project dimension natively (1:1 projects), has no
  issue-count ceiling relevant to this queue, and terminal removal matches
  the pilot's "open tasks only" intent. Costs: a new adapter (this change),
  a config mapping, and the trial clock (~10-04) forcing an exit decision.
- **Linear** needs no new writer — the #174 connector exists — but the 250
  cap with only 35 issues of headroom and ~422 terminal touches/week makes
  it the fragile long-term surface, and hk `project` does not survive as a
  first-class grouping dimension.

**Recommendation**: run the Plane pilot (dry-run-first, live only after
operator-desk enables `HK_PLANE_API_KEY` + `--plane-live`), evaluate against
the exit gate before 2026-10-04, and keep the Linear connector as the
fallback for small queues. The 250-cap headroom (35) is the deciding
structural constraint against Linear for the full queue.
