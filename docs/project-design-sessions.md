# Project design sessions

Every project has one design session that owns its repository. This file
records which lane that is, so no tool has to read hk docs to answer "who owns
project X" and spawners stop typing `--owner` by hand (hk task 3420,
`decision/2026-11-07/design-session-desk-authority`).

## What the field means

`task_projects.design_lane` holds the lane of the project's design session,
for example `auto_trader-design`. An empty value means nobody has claimed the
project. `project_owner_events` is the append-only audit trail: one row per
change with the previous lane, the new lane, the actor (the authenticated
client) and the note.

**Fallback.** The owner of a project is `design_lane` when it is set, else
`operator-desk`. Every response carries both `design_lane` and the resolved
`effective_owner`, so callers never repeat the rule. hk does not check whether
a lane is live; liveness belongs to the panewire hub. The desk takes a project
only when it has no design session.

## API (`/v1`, bearer)

| Route | Meaning |
| --- | --- |
| `GET /v1/tasks/projects/owners` | `{"projects":[{"name","design_lane","effective_owner","updated_at"?}]}` for every configured project, in name order |
| `GET /v1/tasks/projects/{project}/owner` | one project plus its last 20 events |
| `PUT /v1/tasks/projects/{project}/owner` | body `{"design_lane":"auto_trader-design","note":"…"}`; an empty `design_lane` clears the owner |

The actor is the authenticated client name and is never part of the body.
Errors follow the usual contract: `404 not_found` for an unknown project, `400
invalid_context` for a malformed lane. Setting the lane a project already has
is a no-op and writes no event. The existing metadata routes
(`GET|PUT|PATCH …/metadata`) never read or clear `design_lane`.

## CLI

```bash
handoffkeep tasks owners                      # every project's owner
handoffkeep tasks owner auto_trader           # one project with its history
handoffkeep tasks owner auto_trader --set auto_trader-design \
  --note decision/2026-11-07/auto-trader-operator-design-merge
handoffkeep tasks owner auto_trader --clear --note "session closed"
```

Output is JSON. `--set` and `--clear` are mutually exclusive.

**Recording a decision.** An operator decision that reassigns a project is
recorded by the same call, with the decision doc key in the note:

```bash
handoffkeep tasks owner auto_trader-operator --set auto_trader-design \
  --note "decision/2026-11-07/auto-trader-operator-design-merge"
```

The note is the link between the durable decision and the machine-readable
owner, so the history explains itself without reading the CLI's memory.
