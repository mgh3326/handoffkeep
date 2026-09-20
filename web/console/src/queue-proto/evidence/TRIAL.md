# Iteration-1 operator trial protocol (task #462)

Local-only, sanitized. Uses the `stale63` fixture — a deterministic synthetic
63-task set whose non-terminal age distribution is exactly
**6 (<24h) / 14 (1–3d) / 17 (3–7d) / 24 (≥7d)** plus 2 tasks with unmeasurable
age (absent timestamps → rendered `unknown`, never `0`). It is not the
production backlog.

## Setup

```sh
cd web/console
npm ci
npm run build:proto
npx vite preview --config vite.proto.config.ts --port 5199 --strictPort
```

Open `http://localhost:5199/queue-proto.html?set=stale63` and expand the
`measurement panel` at the bottom. Each run records `script`, `condition`,
`outcome`, and elapsed ms. Export JSON stays on the operator's machine.

## Conditions

- `current-like` — the pre-iteration baseline: view selection via the toolbar,
  no view rail.
- `iter1-rail` — this iteration: semantic view rail, grouped dense list,
  stale markers.

Every task is run **twice per operator — once per condition — alternating
order**: operators 1,3,5 run baseline first; operators 2,4 run iter1-rail
first. To get a baseline-like surface in this build, collapse the rail with
the `☰ views` header toggle (record condition `current-like`).

## The five tasks (I1–I5)

Exact fixture answers are in parentheses — the grader checks them, the
operator never sees them.

| script | operator instruction | correct answer |
|--------|----------------------|----------------|
| I1 | Switch to the **All** view using the view navigator and report the shown task count. | `63 unique tasks` |
| I2 | Find the urgent candidate (priority ≥ 90) in this fixture. If no task qualifies, answer `none valid`. | `#6130` (p95) — exactly one qualifies |
| I3 | Find the blocked task and read its blocker evidence in the detail drawer. | `#6150`, blocker: `synth blocker: awaiting external review verdict` |
| I4 | Trace the ownership flow of the handoff task through its history events. | `#6140`: synth-claim-1 → synth-claim-2 → synth-claim-3 |
| I5 | Open any task's detail, close the drawer, and confirm the same row is still selected and in place. | drawer closes; originating row stays focused/selected |

For each run: pick the script in the measurement panel, read the instruction,
`start timer`, perform it, `stop + record` with the outcome
(`correct` / `wrong-task` / `timeout` / `abandoned`).

## Gate (pending — no operator results are fabricated here)

- paired-time-ratio median (iter1 / baseline) ≤ 0.80
- wrong task/ruling selection: 0/10 per operator
- explicit operator yes/no on visual quality and information architecture

## Staleness rule being exercised

A task is marked `⚠ stale` when it is non-terminal and its `created_at` age is
≥ 7 days (`STALE_MIN_AGE_DAYS = 7` in `adapter.ts`). The marker is a
premise-review signal only — it never implies the task is safe to execute.
The drawer repeats that wording next to the stale note.
