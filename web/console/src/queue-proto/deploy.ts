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

/** One scanned document that is not a deploy record, with the reason classes
 * that failed ("key" · "deployed_at" · "schema"; #620 AC4). Unknown reason
 * strings render verbatim — a newer server's extra class must not fail the
 * whole payload. */
export type DeployInvalidRecord = {
  key: string;
  reasons: string[];
};

export type DeployServiceView = {
  service: string;
  repo: string;
  target?: string;
  doc_url: string;
  record_count: number;
  invalid_count?: number;
  invalid?: DeployInvalidRecord[];
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

const MERGED_BOUNDARIES = new Set(["deployed_at", "unrecorded", "no_current"]);
const RECORD_RESULTS = new Set(["success", "failed", "rolled_back"]);

// Every field the panel dereferences is checked, including nullable ones —
// a record missing failed_step must not pass and render "step undefined"
// instead of failing loudly.
const isRecordView = (v: unknown): v is DeployRecordView => {
  if (typeof v !== "object" || v === null) {
    return false;
  }
  const r = v as DeployRecordView;
  return (
    typeof r.record_key === "string" &&
    RECORD_RESULTS.has(r.result) &&
    (r.deployed_ref === null || typeof r.deployed_ref === "string") &&
    (r.deployed_at === null || typeof r.deployed_at === "string") &&
    (r.failed_step === null || typeof r.failed_step === "number") &&
    typeof r.recorded_at === "string" &&
    typeof r.source === "string" &&
    (r.serving_maybe_changed === undefined || typeof r.serving_maybe_changed === "boolean")
  );
};

const isMergedTask = (v: unknown): v is DeployMergedTask => {
  if (typeof v !== "object" || v === null) {
    return false;
  }
  const t = v as DeployMergedTask;
  return (
    typeof t.task_id === "number" &&
    typeof t.title === "string" &&
    typeof t.pr === "string" &&
    typeof t.merged_at === "string"
  );
};

const isInvalidRecord = (v: unknown): v is DeployInvalidRecord => {
  if (typeof v !== "object" || v === null) {
    return false;
  }
  const r = v as DeployInvalidRecord;
  return (
    typeof r.key === "string" &&
    Array.isArray(r.reasons) &&
    r.reasons.length > 0 &&
    r.reasons.every((reason) => typeof reason === "string")
  );
};

const isServiceView = (v: unknown): v is DeployServiceView => {
  if (typeof v !== "object" || v === null) {
    return false;
  }
  const s = v as DeployServiceView;
  return (
    typeof s.service === "string" &&
    typeof s.repo === "string" &&
    typeof s.doc_url === "string" &&
    (s.target === undefined || typeof s.target === "string") &&
    typeof s.record_count === "number" &&
    (s.invalid_count === undefined || typeof s.invalid_count === "number") &&
    (s.invalid === undefined || (Array.isArray(s.invalid) && s.invalid.every(isInvalidRecord))) &&
    (s.docs_capped === undefined || typeof s.docs_capped === "boolean") &&
    MERGED_BOUNDARIES.has(s.merged_boundary) &&
    Array.isArray(s.merged_since) &&
    s.merged_since.every(isMergedTask) &&
    (s.current === null || isRecordView(s.current)) &&
    (s.latest === undefined || isRecordView(s.latest))
  );
};

/** A payload that only matches the outer `services` array is still malformed —
 * nested rows must carry the fields the panel dereferences, or the response is
 * rejected wholesale instead of crashing mid-render. Required top-level fields
 * are checked too: a missing generated_at would silently render without the
 * elapsed baseline the panel needs. */
export function isDeployPendingResponse(body: unknown): body is DeployPendingResponse {
  if (typeof body !== "object" || body === null) {
    return false;
  }
  const b = body as DeployPendingResponse;
  return (
    typeof b.generated_at === "string" &&
    typeof b.pr_source === "string" &&
    (b.events_capped === undefined || typeof b.events_capped === "boolean") &&
    Array.isArray(b.services) &&
    b.services.every(isServiceView)
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
