// Full-page deep link /ui/tasks/<id> — the same DetailBody the peek drawer
// renders, mounted standalone. The task is fetched by id directly, so the
// page works for tasks outside any current filter or list page.

import { useEffect, useState } from "react";
import { fetchTaskDetail } from "../shared/boardApi";
import type { BoardDetail } from "../shared/boardTypes";
import { boardTaskToProto } from "./boardtask";
import { CopyTaskLink, DetailBody } from "./DetailDrawer";
import type { FetchDoc } from "./DocInline";
import { liveCommentsClient, type CommentsClient } from "./TaskComments";
import { KNOWN_STATES, type Dataset } from "./types";

type PageState = "loading" | "loaded" | "notfound" | "error";

export function TaskPage({
  id,
  fetchDetail = fetchTaskDetail,
  fetchDoc,
  comments = liveCommentsClient,
}: {
  id: number;
  fetchDetail?: (id: number) => Promise<BoardDetail>;
  fetchDoc?: FetchDoc;
  comments?: CommentsClient;
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
        <div className="hk-detail-layout">
          <div>
            <DetailBody dataset={dataset} task={boardTaskToProto(detail.task)} detail={{ status: "loaded", data: detail }} fetchDoc={fetchDoc} comments={comments} />
          </div>
          <aside className="hk-properties">
            <h2>Properties</h2>
            <dl>
              <dt>State</dt><dd>{detail.task.state}</dd>
              {detail.task.category ? <><dt>Category</dt><dd>{detail.task.category}</dd></> : null}
              {detail.task.awaiting_decision ? <><dt>Decision</dt><dd>결정 대기 중</dd></> : null}
              <dt>Priority</dt><dd>p{detail.task.priority}</dd>
              <dt>Lane</dt><dd>{detail.task.lane}</dd>
              {detail.task.parent_id != null ? <><dt>Parent</dt><dd>#{detail.task.parent_id}</dd></> : null}
              <dt>Claimant</dt><dd>{detail.task.claimed_by || "Unclaimed"}</dd>
              <dt>Project</dt><dd>{detail.task.project || "Not collected"}</dd>
              <dt>Area</dt><dd>{detail.task.area || "Not collected"}</dd>
              {detail.children && detail.children.length > 0 ? (
                <>
                  <dt>Children</dt>
                  <dd>
                    <ul>
                      {detail.children.map((c) => (
                        <li key={c.id}>
                          <a href={`/ui/tasks/${c.id}`}>#{c.id}</a> {c.title} ({c.state})
                        </li>
                      ))}
                    </ul>
                  </dd>
                </>
              ) : null}
              {detail.relations && detail.relations.length > 0 ? (
                <>
                  <dt>Relations</dt>
                  <dd>
                    <ul>
                      {detail.relations.map((r) => (
                        <li key={r.id}>
                          {r.kind}: <a href={`/ui/tasks/${r.src_id}`}>#{r.src_id}</a> → <a href={`/ui/tasks/${r.dst_id}`}>#{r.dst_id}</a>
                        </li>
                      ))}
                    </ul>
                  </dd>
                </>
              ) : null}
            </dl>
            <a href={`/ui/timeline?lane=${encodeURIComponent(detail.task.lane)}`}>Timeline / events</a>
          </aside>
        </div>
      ) : null}
    </div>
  );
}
