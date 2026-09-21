// Full-page deep link /ui/tasks/<id> — the same DetailBody the peek drawer
// renders, mounted standalone. The task is fetched by id directly, so the
// page works for tasks outside any current filter or list page.

import { useEffect, useState } from "react";
import { fetchTaskDetail } from "../board/api";
import type { BoardDetail } from "../board/types";
import { boardTaskToProto } from "./boardtask";
import { CopyTaskLink, DetailBody } from "./DetailDrawer";
import type { FetchDoc } from "./DocInline";
import { KNOWN_STATES, type Dataset } from "./types";

type PageState = "loading" | "loaded" | "notfound" | "error";

export function TaskPage({
  id,
  fetchDetail = fetchTaskDetail,
  fetchDoc,
}: {
  id: number;
  fetchDetail?: (id: number) => Promise<BoardDetail>;
  fetchDoc?: FetchDoc;
}) {
  const [state, setState] = useState<PageState>("loading");
  const [detail, setDetail] = useState<BoardDetail | null>(null);
  const [dataset, setDataset] = useState<Dataset | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState("loading");
    setDetail(null);
    fetchDetail(id).then(
      (next) => {
        if (cancelled) {
          return;
        }
        setDetail(next);
        setDataset({
          key: "task",
          label: "single task",
          source: "live",
          generatedAt: new Date().toISOString(),
          completeness: "complete",
          completenessNote: `live /ui/api/board/tasks/${id} — single-task fetch`,
          states: [...KNOWN_STATES],
          tasks: [],
          enrichment: {},
        });
        setState("loaded");
      },
      (err) => {
        if (!cancelled) {
          setState((err as { status?: number } | null)?.status === 404 ? "notfound" : "error");
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [id, fetchDetail]);

  return (
    <div className="qp-taskpage">
      <div className="qp-drawer-head">
        <h3>
          #{id} <CopyTaskLink id={id} />
        </h3>
        <div className="qp-drawer-nav">
          <a className="qp-taskpage-queue" href={`/ui/queue?task=${id}`}>
            open in queue
          </a>
        </div>
      </div>
      {state === "loading" ? <p className="muted">loading…</p> : null}
      {state === "notfound" ? (
        <p className="qp-unknown" role="note">
          task #{id} not found — no queue task has this id
        </p>
      ) : null}
      {state === "error" ? <p className="qp-unknown">unavailable — detail fetch failed</p> : null}
      {state === "loaded" && detail !== null && dataset !== null ? (
        <DetailBody dataset={dataset} task={boardTaskToProto(detail.task)} detail={{ status: "loaded", data: detail }} fetchDoc={fetchDoc} />
      ) : null}
    </div>
  );
}
