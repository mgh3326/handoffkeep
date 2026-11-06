import { getJSON } from "../shared/http";
import { readCsrfToken } from "../shared/boardApi";
import type {
  DecisionListData,
  Detail,
  ResolveBody,
  SubmitResult,
  Tab,
} from "./types";

function obj(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
function str(v: unknown): v is string {
  return typeof v === "string";
}
function num(v: unknown): v is number {
  return typeof v === "number" && Number.isFinite(v);
}
function nullableString(v: unknown): boolean {
  return v === null || str(v);
}
function optionalString(v: unknown): boolean {
  return v === undefined || str(v);
}
function record(v: unknown): boolean {
  return (
    obj(v) &&
    num(v.id) &&
    str(v.source) &&
    str(v.source_ref) &&
    (v.task_id === null || num(v.task_id)) &&
    str(v.lane) &&
    str(v.question) &&
    nullableString(v.due_at) &&
    typeof v.overdue === "boolean" &&
    str(v.status) &&
    str(v.created_at) &&
    nullableString(v.answered_at) &&
    str(v.superseded_by) &&
    str(v.answer) &&
    (v.superseded_record_id === undefined ||
      v.superseded_record_id === null ||
      num(v.superseded_record_id))
  );
}
function request(v: unknown): boolean {
  return (
    obj(v) &&
    str(v.id) &&
    num(v.revision) &&
    str(v.state) &&
    str(v.question) &&
    Array.isArray(v.options) &&
    v.options.every(
      (x) =>
        obj(x) &&
        str(x.key) &&
        str(x.label) &&
        (x.recommended === undefined || typeof x.recommended === "boolean"),
    ) &&
    typeof v.allow_free === "boolean" &&
    str(v.default_action) &&
    [
      v.default_option,
      v.default_trigger,
      v.due_at,
      v.doc,
      v.superseded_by,
    ].every(optionalString) &&
    (v.refs === undefined || (Array.isArray(v.refs) && v.refs.every(str))) &&
    (v.resolution === undefined ||
      (obj(v.resolution) &&
        str(v.resolution.kind) &&
        [v.resolution.option, v.resolution.text, v.resolution.receipt].every(
          optionalString,
        )))
  );
}
export async function loadList(tab: Tab): Promise<DecisionListData> {
  const v = await getJSON<unknown>(`/ui/api/decisions?tab=${tab}`);
  if (
    !obj(v) ||
    !str(v.generated_at) ||
    v.tab !== tab ||
    !obj(v.counts) ||
    !num(v.counts.open) ||
    !num(v.counts.overdue) ||
    typeof v.truncated !== "boolean" ||
    !Array.isArray(v.items) ||
    !v.items.every(
      (x) =>
        record(x) &&
        obj(x) &&
        str(x.request_state) &&
        str(x.request_id) &&
        num(x.revision),
    )
  )
    throw new Error("malformed decision list");
  return v as DecisionListData;
}
export async function loadDetail(id: string): Promise<Detail> {
  const v = await getJSON<unknown>(
    `/ui/api/decisions/${encodeURIComponent(id)}`,
  );
  if (
    !obj(v) ||
    !str(v.generated_at) ||
    !record(v.record) ||
    !(
      v.task === null ||
      (obj(v.task) &&
        num(v.task.id) &&
        str(v.task.title) &&
        str(v.task.state) &&
        str(v.task.lane) &&
        (v.task.project === undefined || nullableString(v.task.project)))
    ) ||
    !(
      v.request === null ||
      (request(v.request) &&
        obj(v.request) &&
        typeof v.request.human_only === "boolean" &&
        str(v.request.answer_policy))
    ) ||
    !Array.isArray(v.history) ||
    !v.history.every(request) ||
    !Array.isArray(v.attempts) ||
    !v.attempts.every(
      (x) =>
        obj(x) &&
        num(x.id) &&
        str(x.channel) &&
        str(x.provenance) &&
        str(x.actor) &&
        str(x.result) &&
        str(x.revision_request_id) &&
        num(x.question_revision) &&
        str(x.received_at),
    ) ||
    typeof v.answerable !== "boolean" ||
    !str(v.disabled_reason) ||
    (v.answerable && (v.request === null || v.disabled_reason !== ""))
  )
    throw new Error("malformed decision detail");
  return v as Detail;
}
export async function postResolve(
  body: ResolveBody,
): Promise<{ status: number; result: SubmitResult; raw: string }> {
  const csrf = readCsrfToken();
  if (!csrf) throw new Error("missing csrf");
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), 15_000);
  try {
    const response = await fetch("/ui/api/decision-requests/resolve", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body),
      signal: controller.signal,
    });
    const v: unknown = await response.json();
    if (!obj(v) || !str(v.result))
      return {
        status: response.status,
        result: "unknown",
        raw: obj(v) && str(v.error) ? v.error : "unknown",
      };
    const known: string[] = [
      "answered",
      "conflict",
      "stale_revision",
      "pending_confirmation",
      "expired",
      "superseded",
    ];
    return {
      status: response.status,
      result: known.includes(v.result) ? (v.result as SubmitResult) : "unknown",
      raw: v.result,
    };
  } finally {
    window.clearTimeout(timer);
  }
}
