import { useCallback, useEffect, useMemo, useState } from "react";
import { Icon, IconFrame } from "../shared/Icon";
import { StateIcon } from "../shared/StateIcon";
import { readStored, writeStored } from "../shared/storage";
import { formatMinuteUTC } from "../shared/time";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { usePoll } from "../shared/usePoll";
import { BULK_LIMIT, TriageUi, useInboxTriage, type Triage, type TriageRow } from "../shared/actions/InboxTriage";
import { writeInbox } from "../shared/inboxApi";
import { loadAttention } from "./api";
import type { Attention, AttentionDecision, AttentionDeploy, AttentionTask } from "./types";
import { fetchTaskDetail } from "../shared/boardApi";
import { TaskActions } from "../shared/actions/TaskActions";
import type { ActionTarget } from "../shared/actions/index";
import type { BoardTask } from "../shared/boardTypes";
import { QueryBar } from "../shared/query/QueryBar";
import { parseQuery, type QueryContext, type QueryFilters } from "../shared/query/query";
import "./home.css";

// The old browser-local seen list. It is migrated once into the server's
// inbox state and never written again (hk 3343 §3).
const seenKey = "hk-console:home-seen";
const seenMigratedKey = "hk-console:home-seen-migrated";

function readSeen(): string[] {
  try {
    const value: unknown = JSON.parse(readStored(seenKey) ?? "[]");
    return Array.isArray(value) ? value.filter((x): x is string => typeof x === "string") : [];
  } catch { return []; }
}
export function summarizeQuestion(text: string): string {
  const line = text.split(/\r?\n/)[0].replace(/[#*_`~\[\]]/g, "").replace(/\s+/g, " ").trim();
  const match = /[:.!?]/.exec(line);
  const end = match ? match.index + (match[0] === ":" ? 0 : 1) : line.length;
  const Segmenter = (Intl as unknown as { Segmenter: new (locale: string, options: { granularity: "grapheme" }) => { segment: (text: string) => Iterable<{ segment: string }> } }).Segmenter;
  const chars = Array.from(new Segmenter("ko", { granularity: "grapheme" }).segment(line.slice(0, end)), x => x.segment);
  return chars.length > 90 ? `${chars.slice(0, 90).join("")}…` : chars.join("");
}
function age(now: string, iso: string): string {
  const minutes = Math.max(0, Math.floor((Date.parse(now) - Date.parse(iso)) / 60000));
  if (!Number.isFinite(minutes)) return "시각 미상";
  if (minutes < 1) return "방금";
  if (minutes < 60) return `${minutes}분`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}시간`;
  return `${Math.floor(minutes / 1440)}일`;
}
function dateGroup(now: string, iso: string): string {
  const day = (s: string) => { const d = new Date(s); return Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()); };
  const days = Math.floor((day(now) - day(iso)) / 86400000);
  if (days <= 0) return "오늘";
  if (days === 1) return "어제";
  const weekday = new Date(now).getDay() || 7;
  if (days < weekday) return "이번 주";
  if (days < weekday + 7) return "지난주";
  return "이전";
}
type Item =
  | { kind: "decision"; value: AttentionDecision; time: string; key: string; overdue: boolean; read: boolean; woken: boolean; task?: ActionTarget }
  | { kind: "task"; value: AttentionTask; time: string; key: string; overdue: false; read: boolean; woken: boolean; task: ActionTarget }
  | { kind: "deploy"; value: AttentionDeploy; time: string; key: string; overdue: false; read: boolean; woken: boolean; task?: ActionTarget };

/** The inbox rows in the order the page renders them: action rows first
 * (overdue first, then newest), then the reference rows (newest). The
 * keyboard walks this same list. */
function attentionItems(data: Attention): { action: Item[]; fyi: Item[] } {
  const action: Item[] = [
    ...data.decisions.items.map((value): Item => ({ kind: "decision", value, time: value.created_at, key: value.key, overdue: value.overdue, read: value.read, woken: value.woken })),
    ...data.tasks.items.map((value): Item => ({ kind: "task", value, time: value.since, key: value.key, overdue: false, read: value.read, woken: value.woken, task: { id: value.id, state: value.state, priority: value.priority, title: value.title, lane: value.lane } })),
  ].sort((a, b) => Number(b.overdue) - Number(a.overdue) || Date.parse(b.time) - Date.parse(a.time));
  const fyi: Item[] = data.deploys.items
    .map((value): Item => ({ kind: "deploy", value, time: value.merged_at, key: value.key, overdue: false, read: value.read, woken: value.woken }))
    .sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
  return { action, fyi };
}

function triageRow(item: Item): TriageRow {
  return { key: item.key, read: item.read, done: false, woken: item.woken, task: item.task };
}

function itemLane(item: Item): string {
  return item.kind === "deploy" ? item.value.service : item.value.lane;
}

function itemState(item: Item): string {
  return item.kind === "task" ? item.value.state : item.kind === "decision" ? "awaiting_decision" : "merged";
}

function itemText(item: Item): string {
  if (item.kind === "decision") return `${item.value.id} ${item.value.question}`;
  if (item.kind === "task") return `${item.value.id} ${item.value.title}`;
  return `${item.value.task_id} ${item.value.title} ${item.value.pr} ${item.value.service}`;
}

/** Home has no project, task-kind, grade or tier field, so those filters hide
 * everything instead of pretending to match. The page says so. */
const HOME_UNSUPPORTED = ["project", "kind", "grade", "tier"] as const;

function filterItems(items: { action: Item[]; fyi: Item[] }, filters: QueryFilters): { action: Item[]; fyi: Item[] } {
  const needle = filters.text.trim().toLowerCase();
  const match = (item: Item): boolean => {
    if (needle && !itemText(item).toLowerCase().includes(needle)) return false;
    if (filters.lane && itemLane(item) !== filters.lane) return false;
    if (filters.state && itemState(item) !== filters.state) return false;
    if (filters.priority) {
      if (item.kind !== "task" || item.value.priority < Number(filters.priority)) return false;
    }
    if (HOME_UNSUPPORTED.some(key => filters[key] !== "")) return false;
    if (filters.view === "needs-me" && item.kind === "deploy") return false;
    if (filters.view === "active" && !(item.kind === "task" && ["researching", "developing", "reviewing"].includes(item.value.state))) return false;
    return true;
  };
  return { action: items.action.filter(match), fyi: items.fyi.filter(match) };
}

function Row({ item, now, selected, toggle }: { item: Item; now: string; selected: boolean; toggle: (key: string) => void }) {
  let href: string, id: number, title: string, summary: string;
  let icon: React.ReactNode, chips: React.ReactNode;
  if (item.kind === "decision") {
    const x = item.value;
    href = x.source === "request" ? `/ui/decisions/${x.id}` : "/ui/decisions";
    id = x.id; title = summarizeQuestion(x.question) || "질문 없음"; summary = "결정 대기";
    icon = <StateIcon state="awaiting_decision" />;
    chips = <><Chip kind="status" state="awaiting_decision" label={x.source === "request" ? "요청" : "알림"} />{x.overdue ? <Chip kind="status" state="stalled" stalled label="기한 경과" /> : x.due_at ? <Chip kind="status" state="triage" label={`기한 ${age(x.due_at, now)} 남음`} /> : null}<Chip kind="lane" lane={x.lane} /></>;
  } else if (item.kind === "task") {
    const x = item.value;
    href = `/ui/tasks/${x.id}`; id = x.id; title = x.title; summary = x.reason === "stalled" ? "멈춘 작업" : "결정 대기 작업";
    icon = x.reason === "stalled" ? <Icon name="warning" /> : <StateIcon state={x.state} />;
    chips = <><Chip kind="status" state={x.state} stalled={x.reason === "stalled"} label={x.reason === "stalled" ? "멈춤" : undefined} /><Chip kind="lane" lane={x.lane} /></>;
  } else {
    const x = item.value;
    href = `/ui/tasks/${x.task_id}`; id = x.task_id; title = x.title; summary = "머지 후 미배포";
    icon = <StateIcon state="merged" />; chips = <Chip kind="lane" lane={x.service} />;
  }
  return <li data-action-task={item.kind === "task" ? item.value.id : undefined} className={`ha-item action-row${item.read ? " ha-item--seen" : ""}${selected ? " ha-item--selected" : ""}`}>
    <a className="ha-row" href={href} data-inbox-row data-inbox-key={item.key} data-selected={selected ? "true" : undefined}>
      <span className="ha-icon">{icon}</span><span className="ha-content">
        <span className="ha-line"><span className="ha-dot" aria-hidden="true" /><span className="hk-sr-only">{item.read ? "읽음" : "읽지 않음"}</span><Mono>#{id}</Mono><span className="ha-row-title">{title}</span><time dateTime={item.time} title={formatMinuteUTC(item.time) ?? "시각 미상"}>{age(now, item.time)}</time></span>
        <span className="ha-detail"><span className="ha-summary">{summary}</span><span className="ha-chips">{chips}{item.woken ? <Chip kind="status" state="triage" label="다시 알림" /> : null}</span></span>
      </span>
    </a>
    <button type="button" className="inbox-select" aria-pressed={selected} aria-label={`${title} 선택`} onClick={() => toggle(item.key)}>✓</button>
    {item.kind === "task" && <HomeTaskAction id={item.value.id} />}
  </li>;
}
function HomeTaskAction({ id }: { id: number }) {
  const [task, setTask] = useState<BoardTask | null>(null);
  const [failed, setFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => { let active = true; void fetchTaskDetail(id).then(data => { if (active) setTask(data.task); }).catch(() => { if (active) setFailed(true); }); return () => { active = false; }; }, [id, attempt]);
  if (failed) return <span role="alert">작업을 불러오지 못했습니다.</span>;
  return task && <TaskActions compact target={task} onPriority={priority => setTask(old => old ? { ...old, priority } : old)} onApplied={() => setAttempt(value => value + 1)} />;
}
function Body({ data, triage, items, queryActive, unsupported }: { data: Attention; triage: Triage; items: { action: Item[]; fyi: Item[] }; queryActive: boolean; unsupported: boolean }) {
  if (!(data.decisions.count + data.tasks.count + data.deploys.count)) return <div className="ha-empty"><IconFrame><path d="M4 5h16v14H4zM4 14h5l2 2h2l2-2h5" /></IconFrame><p>대기 중인 일 없음</p></div>;
  if (queryActive && items.action.length + items.fyi.length === 0) return <p className="ha-note">조건에 맞는 항목이 없습니다.</p>;
  const section = (title: string, list: Item[], count: number) => {
    const groups: { label: string; rows: Item[] }[] = [];
    for (const item of list) {
      const label = item.overdue ? "기한 지남" : dateGroup(data.generated_at, item.time);
      const last = groups[groups.length - 1];
      if (last?.label === label) last.rows.push(item);
      else groups.push({ label, rows: [item] });
    }
    return <section className="ha-section" aria-label={title}><h2>{title} <span>{count}</span></h2>{groups.map((group, index) => <div key={`${group.label}:${index}`}><h3>{group.label}</h3><ul className="ha-list">{group.rows.map(item => <Row key={item.key} item={item} now={data.generated_at} selected={triage.selected.has(item.key)} toggle={triage.toggle} />)}</ul></div>)}{count > list.length ? <p className="ha-note">+{count - list.length}개 더 있음</p> : null}</section>;
  };
  return <><p className="ha-summary-count">결정 {data.decisions.count} (기한 경과 {data.decisions.overdue}) · 작업 {data.tasks.count} · 배포 대기 {data.deploys.count}</p>{queryActive ? <p className="ha-note">현재 목록에서 필터한 결과입니다.</p> : null}{unsupported ? <p className="ha-note">프로젝트·종류·등급·티어 필터는 작업 화면에서만 적용됩니다.</p> : null}{section("지금 할 일", items.action, queryActive ? items.action.length : data.decisions.count + data.tasks.count)}{section("참고", items.fyi, queryActive ? items.fyi.length : data.deploys.count)}{data.deploys.events_capped ? <p className="ha-note">조회 범위에 제한이 있어 목록이 불완전할 수 있습니다.</p> : null}</>;
}
export function HomeAttention() {
  const [refresh, setRefresh] = useState(0);
  const [query, setQuery] = useState("");
  const load = useCallback(() => loadAttention(), [refresh]);
  const { data, failed } = usePoll(load, 60_000);
  const reload = useCallback(() => setRefresh(value => value + 1), []);
  useEffect(() => { const onChange = () => setRefresh(value => value + 1); window.addEventListener("hk-task-changed", onChange); return () => window.removeEventListener("hk-task-changed", onChange); }, []);
  useSeenMigration(reload);
  const parsedQuery = parseQuery(query);
  const items = useMemo(() => (data ? filterItems(attentionItems(data), parseQuery(query).filters) : { action: [], fyi: [] }), [data, query]);
  const queryContext: QueryContext = {
    lanes: data ? [...new Set([...data.decisions.items.map(item => item.lane), ...data.tasks.items.map(item => item.lane), ...data.deploys.items.map(item => item.service)])].sort() : [],
    projects: [],
    kinds: [],
    grades: [],
    tiers: [],
  };
  const unsupported = HOME_UNSUPPORTED.some(key => parsedQuery.filters[key] !== "");
  const rows = useMemo(() => [...items.action, ...items.fyi].map(triageRow), [items]);
  const triage = useInboxTriage(rows, reload);
  return <div className="ha-root"><h1 className="ha-title">나를 기다리는 일</h1><QueryBar value={query} onChange={setQuery} errors={query.trim() === "" ? [] : parsedQuery.errors} context={queryContext} /><TriageUi triage={triage} />{failed ? <p className="ha-error" role="alert">목록을 불러오지 못했습니다{data ? " · 마지막으로 불러온 데이터를 표시합니다" : ""}.</p> : null}{data ? <Body data={data} triage={triage} items={items} queryActive={query.trim() !== ""} unsupported={unsupported} /> : failed ? null : <p role="status">불러오는 중…</p>}</div>;
}

/** Moves the browser-local seen list into the server once, then leaves the
 * key out of localStorage for good. A failed send is retried on the next
 * load; the deterministic event keys make a retry a replay. */
function useSeenMigration(reload: () => void) {
  useEffect(() => {
    if (readStored(seenMigratedKey) === "1") return;
    const raw = readStored(seenKey);
    if (raw === null) return;
    const keys = readSeen();
    let cancelled = false;
    const finish = () => {
      if (cancelled) return;
      writeStored(seenMigratedKey, "1");
      try { localStorage.removeItem(seenKey); } catch { /* storage blocked */ }
      reload();
    };
    void (async () => {
      for (let index = 0; index < keys.length; index += BULK_LIMIT) {
        try {
          await writeInbox("read", keys.slice(index, index + BULK_LIMIT), `inbox-seen-migrate-${index / BULK_LIMIT}`);
        } catch {
          return;
        }
      }
      finish();
    })();
    return () => { cancelled = true; };
  }, [reload]);
}
