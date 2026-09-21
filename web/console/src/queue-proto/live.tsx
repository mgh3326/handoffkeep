// Live wiring: the production queue entry fetches the real board API and maps
// BoardTask rows onto ProtoTask. Fields the list API does not carry stay
// absent — null/[]/"not_collected" — and render as unknown, never as 0, false,
// or a blank. Detail-only fields (events, dwell, coverage) are fetched lazily
// per open drawer via BoardDetail, never per list row.

import { useEffect, useState } from "react";
import { fetchBoardTasks, fetchTaskDetail } from "../board/api";
import type { BoardTask } from "../board/types";
import { QueueProtoApp } from "./QueueProtoApp";
import type { Dataset, ProtoTask } from "./types";

/** BoardTask → ProtoTask. The five fields the list API does not expose enter
 * as the type contract demands: null / [] / not_collected. */
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
    refs: task.refs,
    events: [],
    dwell: [],
    coverage: { status: "not_collected", participants: null },
  };
}

/** Fetches the whole queue via the board BFF cursor walk and packages it as a
 * live Dataset. generatedAt is the server snapshot timestamp — the honest
 * "now" for staleness, not the client's clock. */
export async function fetchLiveDataset(): Promise<Dataset> {
  const board = await fetchBoardTasks();
  return {
    key: "live",
    label: "live queue backlog",
    source: "live",
    generatedAt: board.generatedAt,
    completeness: board.truncated ? "partial" : "complete",
    completenessNote: board.truncated
      ? "live /ui/api/board/tasks — truncated at the client page cap; shown rows are a subset"
      : "live /ui/api/board/tasks — every page walked to the cursor end",
    tasks: board.tasks.map(boardTaskToProto),
    enrichment: {},
  };
}

/** Production mount: renders the app only once the live dataset has loaded —
 * live data only. A fetch failure renders an explicit error, never a fixture
 * and never a fabricated dataset. */
export function LiveQueue({ loadDataset = fetchLiveDataset }: { loadDataset?: () => Promise<Dataset> }) {
  const [dataset, setDataset] = useState<Dataset | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    loadDataset().then(
      (next) => {
        if (!cancelled) {
          setDataset(next);
        }
      },
      (err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [loadDataset]);
  if (error !== null) {
    return (
      <p className="qp-load-error" role="alert">
        queue unavailable — {error}
      </p>
    );
  }
  if (dataset === null) {
    return <p className="muted">loading queue…</p>;
  }
  return <QueueProtoApp datasets={{ live: dataset }} initialSet="live" fetchDetail={fetchTaskDetail} />;
}
