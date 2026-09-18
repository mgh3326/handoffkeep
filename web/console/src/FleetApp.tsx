import { useEffect, useState } from "react";

type FleetSession = {
  pane_id: string;
  workspace_id: string;
  label: string;
  status: string;
  interactive_ready?: boolean;
  model: string;
};

type FleetNode = {
  machine_id: string;
  state: string;
  last_ping: number | null;
  sessions: FleetSession[];
  snapshot_status: string;
  truncated: boolean;
  received_at: string;
  stale: boolean;
};

type FleetResponse = {
  fetched_at: string;
  upstream: string;
  nodes: FleetNode[];
};

const FLEET_API = "/ui/api/fleet";
const POLL_MS = 10_000;

function nodeBadges(node: FleetNode): string[] {
  const badges: string[] = [];
  if (node.snapshot_status === "unavailable") {
    badges.push("수집 실패");
  } else if (node.sessions.length === 0) {
    badges.push("빈 목록");
  }
  if (node.stale || node.state === "stale") {
    badges.push("stale");
  }
  if (node.truncated) {
    badges.push("잘림(64)");
  }
  return badges;
}

export function FleetApp() {
  const [data, setData] = useState<FleetResponse | null>(null);
  const [firstLoad, setFirstLoad] = useState(true);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const response = await fetch(FLEET_API);
        if (!response.ok) {
          throw new Error("fleet request failed");
        }
        const next = (await response.json()) as FleetResponse;
        if (!cancelled) {
          setData(next);
        }
      } catch {
        // Keep the last successful payload, including original received_at.
      } finally {
        if (!cancelled) {
          setFirstLoad(false);
        }
      }
    };
    void load();
    const timer = window.setInterval(() => {
      void load();
    }, POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  if (firstLoad && data === null) {
    return <p className="muted">불러오는 중</p>;
  }
  if (data === null) {
    return <p className="muted">세션표를 아직 받지 못했습니다.</p>;
  }

  return (
    <section>
      <h2>Fleet sessions</h2>
      <div className="fleet-status">
        <span>
          마지막 성공 시각 <time>{data.fetched_at || "없음"}</time>
        </span>
        <span>
          상류 <span className="badge">{data.upstream}</span>
        </span>
      </div>
      <table className="fleet-table">
        <thead>
          <tr>
            <th>머신</th>
            <th>상태</th>
            <th>세션</th>
            <th>pane</th>
            <th>status</th>
            <th>interactive_ready</th>
            <th>received_at</th>
            <th>모델</th>
          </tr>
        </thead>
        <tbody>
          {data.nodes.length === 0 ? (
            <tr>
              <td colSpan={8} className="muted">
                표시할 노드가 없습니다.
              </td>
            </tr>
          ) : (
            data.nodes.flatMap((node) => {
              const badges = nodeBadges(node);
              if (node.sessions.length === 0) {
                return [
                  <tr key={node.machine_id}>
                    <td className="fleet-machine">{node.machine_id}</td>
                    <td>
                      {badges.map((badge) => (
                        <span key={badge} className={badge === "stale" ? "badge stale" : "badge"}>
                          {badge}
                        </span>
                      ))}
                      <span className="muted"> {node.state}</span>
                    </td>
                    <td colSpan={4} className="muted">
                      {node.snapshot_status === "unavailable" ? "수집 실패" : "세션 없음"}
                    </td>
                    <td>
                      <time>{node.received_at}</time>
                    </td>
                    <td>미수집</td>
                  </tr>,
                ];
              }
              return node.sessions.map((session, index) => (
                <tr key={`${node.machine_id}:${session.pane_id}:${index}`}>
                  {index === 0 ? (
                    <>
                      <td className="fleet-machine" rowSpan={node.sessions.length}>
                        {node.machine_id}
                      </td>
                      <td rowSpan={node.sessions.length}>
                        {badges.map((badge) => (
                          <span key={badge} className={badge === "stale" ? "badge stale" : "badge"}>
                            {badge}
                          </span>
                        ))}
                        <span className="muted"> {node.state}</span>
                      </td>
                    </>
                  ) : null}
                  <td>{session.label}</td>
                  <td>{session.pane_id}</td>
                  <td>{session.status}</td>
                  <td>{session.interactive_ready == null ? "" : String(session.interactive_ready)}</td>
                  <td>
                    <time>{node.received_at}</time>
                  </td>
                  <td>{session.model}</td>
                </tr>
              ));
            })
          )}
        </tbody>
      </table>
    </section>
  );
}
