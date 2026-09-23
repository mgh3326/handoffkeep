// #529 deploy-pending surface — types, fetch and pure format helpers for the
// per-service "last deploy + merged since" block. The server assembles the
// view from deploy-record/v0 documents and merged task_events; the panel
// renders exactly what it is given — "기록 없음" stays "기록 없음", never a
// fabricated "latest" (hk:doc design/2026-09-21/deploy-pending-surface §4).

export type DeployRecordView = {
  record_key: string;
  result: string;
  deployed_ref: string | null;
  deployed_at: string | null;
  failed_step: number | null;
  recorded_at: string;
  source: string;
  serving_maybe_changed?: boolean;
};

export type DeployMergedTask = {
  task_id: number;
  title: string;
  pr: string;
  merged_at: string;
};

export type DeployServiceView = {
  service: string;
  repo: string;
  target?: string;
  doc_url: string;
  record_count: number;
  invalid_count?: number;
  docs_capped?: boolean;
  current: DeployRecordView | null;
  latest?: DeployRecordView;
  merged_boundary: "deployed_at" | "unrecorded" | "no_current";
  merged_since: DeployMergedTask[];
};

export type DeployPendingResponse = {
  generated_at: string;
  pr_source: string;
  services: DeployServiceView[];
  events_capped?: boolean;
};

const isRecordView = (v: unknown): v is DeployRecordView =>
  typeof v === "object" && v !== null &&
  typeof (v as DeployRecordView).record_key === "string" &&
  typeof (v as DeployRecordView).result === "string";

const isServiceView = (v: unknown): v is DeployServiceView =>
  typeof v === "object" && v !== null &&
  typeof (v as DeployServiceView).service === "string" &&
  typeof (v as DeployServiceView).repo === "string" &&
  typeof (v as DeployServiceView).record_count === "number" &&
  typeof (v as DeployServiceView).merged_boundary === "string" &&
  Array.isArray((v as DeployServiceView).merged_since) &&
  ((v as DeployServiceView).current === null || isRecordView((v as DeployServiceView).current)) &&
  ((v as DeployServiceView).latest === undefined || isRecordView((v as DeployServiceView).latest));

/** A payload that only matches the outer `services` array is still malformed —
 * nested rows must carry the fields the panel dereferences, or the response is
 * rejected wholesale instead of crashing mid-render. */
export function isDeployPendingResponse(body: unknown): body is DeployPendingResponse {
  return (
    typeof body === "object" &&
    body !== null &&
    Array.isArray((body as DeployPendingResponse).services) &&
    (body as DeployPendingResponse).services.every(isServiceView)
  );
}

export async function fetchDeployPending(): Promise<DeployPendingResponse> {
  const response = await fetch("/ui/api/deploy-pending");
  if (!response.ok) {
    throw new Error(`deploy-pending ${response.status}`);
  }
  const body: unknown = await response.json();
  if (!isDeployPendingResponse(body)) {
    throw new Error("deploy-pending: malformed payload");
  }
  return body;
}

/** Short form for a deployed ref: 40-hex SHAs and sha256 digests truncate;
 * version strings (pw-e401923) and anything else render verbatim — a ref is
 * never invented or padded. */
export function shortDeployedRef(ref: string | null): string | null {
  if (ref === null) {
    return null;
  }
  if (/^[0-9a-f]{40}$/i.test(ref)) {
    return ref.slice(0, 7);
  }
  const digest = /^sha256:([0-9a-f]{64})$/i.exec(ref);
  if (digest) {
    return `sha256:${digest[1].slice(0, 12)}…`;
  }
  return ref;
}

/** Compact elapsed label measured from the server's generated_at, not the
 * client clock. Unknown input → null (rendered as an explicit gap). */
export function deployElapsed(nowIso: string, iso: string | null): string | null {
  if (iso === null) {
    return null;
  }
  const ms = Date.parse(nowIso) - Date.parse(iso);
  if (!Number.isFinite(ms) || ms < 0) {
    return null;
  }
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) {
    return `${minutes}분 전`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return `${hours}시간 전`;
  }
  return `${Math.floor(hours / 24)}일 전`;
}

/** ISO instant → local "MM-DD HH:MM" for the record line; unparseable input
 * stays null instead of rendering a guess. */
export function deployStamp(iso: string | null): string | null {
  if (iso === null) {
    return null;
  }
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return null;
  }
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** PR number from a github pull URL for display; the href stays the full URL. */
export function deployPRLabel(pr: string): string {
  const m = /\/pull\/(\d+)$/.exec(pr);
  return m ? `#${m[1]}` : pr;
}
