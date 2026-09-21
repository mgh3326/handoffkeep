// BoardTask → ProtoTask mapping, shared by the live queue list and the
// deep-link paths (?task= and /ui/tasks/<id>) where the task may not be in
// the list dataset.

import type { BoardTask } from "../board/types";
import type { ProtoTask } from "./types";

/** The five fields the list API does not expose enter as the type contract
 * demands: null / [] / not_collected. */
export function boardTaskToProto(task: BoardTask): ProtoTask {
  return {
    id: task.id,
    title: task.title,
    kind: task.kind,
    state: task.state,
    lane: task.lane,
    claimant: task.claimed_by ?? null,
    priority: task.priority,
    created_at: task.created_at,
    state_entered_at: null,
    due_at: null,
    blocker: null,
    created_by: task.created_by,
    updated_at: task.updated_at ?? null,
    parent_lane: task.parent_lane ?? null,
    refs: task.refs,
    ...(task.body_doc ? { body_doc: task.body_doc } : {}),
    events: [],
    dwell: [],
    coverage: { status: "not_collected", participants: null },
  };
}
