import { useState } from "react";
import { Icon, IconFrame } from "../shared/Icon";
import { StateIcon } from "../shared/StateIcon";
import { readStored, writeStored } from "../shared/storage";
import { stateLabel } from "../shared/states";
import { formatMinuteUTC } from "../shared/time";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { usePoll } from "../shared/usePoll";
import { loadAttention } from "./api";
import type { Attention, AttentionDecision, AttentionDeploy, AttentionTask } from "./types";
import "./home.css";

const seenKey = "hk-console:home-seen";
function readSeen(): string[] {
  try {
    const value: unknown = JSON.parse(readStored(seenKey) ?? "[]");
    return Array.isArray(value) ? value.filter((x): x is string => typeof x === "string") : [];
  } catch { return []; }
}
export function summarizeQuestion(text: string): string {
  const line = text.split(/\r?\n/)[0].replace(/[#*_`~\[\]]/g, "").replace(/\s+/g, " ").trim();
  const match = /[.!?:](?=\s|$)/.exec(line);
  const end = match ? match.index + (match[0] === ":" ? 0 : 1) : line.length;
  const Segmenter = (Intl as unknown as { Segmenter?: new (locale: string, options: { granularity: "grapheme" }) => { segment: (text: string) => Iterable<{ segment: string }> } }).Segmenter;
  const excerpt = line.slice(0, end);
  // Older browsers fall back to code points, which may split a grapheme at the limit.
  const chars = Segmenter ? Array.from(new Segmenter("ko", { granularity: "grapheme" }).segment(excerpt), x => x.segment) : Array.from(excerpt);
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
  | { kind: "decision"; value: AttentionDecision; time: string; key: string; overdue: boolean }
  | { kind: "task"; value: AttentionTask; time: string; key: string; overdue: false }
  | { kind: "deploy"; value: AttentionDeploy; time: string; key: string; overdue: false };
function Row({ item, now, seen, markSeen }: { item: Item; now: string; seen: boolean; markSeen: (key: string) => void }) {
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
    chips = <><Chip kind="status" state={x.state} stalled={x.reason === "stalled"} awaiting_decision={x.reason === "awaiting_decision"} label={x.reason === "stalled" ? "멈춤" : stateLabel("awaiting_decision")} /><Chip kind="lane" lane={x.lane} /></>;
  } else {
    const x = item.value;
    href = `/ui/tasks/${x.task_id}`; id = x.task_id; title = x.title; summary = "머지 후 미배포";
    icon = <StateIcon state="merged" />; chips = <Chip kind="lane" lane={x.service} />;
  }
  return <li className={`ha-item${seen ? " ha-item--seen" : ""}`}><a className="ha-row" href={href} onClick={() => markSeen(item.key)}>
    <span className="ha-icon">{icon}</span><span className="ha-content">
      <span className="ha-line"><span className="ha-dot" aria-hidden="true" /><span className="hk-sr-only">{seen ? "읽음" : "읽지 않음"}</span><Mono>#{id}</Mono><span className="ha-row-title">{title}</span><time dateTime={item.time} title={formatMinuteUTC(item.time) ?? "시각 미상"}>{age(now, item.time)}</time></span>
      <span className="ha-detail"><span className="ha-summary">{summary}</span><span className="ha-chips">{chips}</span></span>
    </span></a></li>;
}
function Body({ data }: { data: Attention }) {
  const [seen, setSeen] = useState(readSeen);
  const markSeen = (key: string) => { const next = [...new Set([...seen, key])].slice(-500); setSeen(next); writeStored(seenKey, JSON.stringify(next)); };
  const action: Item[] = [
    ...data.decisions.items.map((value): Item => ({ kind: "decision", value, time: value.created_at, key: `decision:${value.source}:${value.id}`, overdue: value.overdue })),
    ...data.tasks.items.map((value): Item => ({ kind: "task", value, time: value.since, key: `task:${value.id}`, overdue: false })),
  ].sort((a, b) => Number(b.overdue) - Number(a.overdue) || Date.parse(b.time) - Date.parse(a.time));
  const fyi: Item[] = data.deploys.items.map((value): Item => ({ kind: "deploy", value, time: value.merged_at, key: `deploy:${value.service}:${value.task_id}`, overdue: false })).sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
  if (!(data.decisions.count + data.tasks.count + data.deploys.count)) return <div className="ha-empty"><IconFrame><path d="M4 5h16v14H4zM4 14h5l2 2h2l2-2h5" /></IconFrame><p>대기 중인 일 없음</p></div>;
  const section = (title: string, items: Item[], count: number) => {
    const groups: { label: string; rows: Item[] }[] = [];
    for (const item of items) {
      const label = item.overdue ? "기한 지남" : dateGroup(data.generated_at, item.time);
      const last = groups[groups.length - 1];
      if (last?.label === label) last.rows.push(item);
      else groups.push({ label, rows: [item] });
    }
    return <section className="ha-section" aria-label={title}><h2>{title} <span>{count}</span></h2>{groups.map((group, index) => <div key={`${group.label}:${index}`}><h3>{group.label}</h3><ul className="ha-list">{group.rows.map(item => <Row key={item.key} item={item} now={data.generated_at} seen={seen.includes(item.key)} markSeen={markSeen} />)}</ul></div>)}{count > items.length ? <p className="ha-note">+{count - items.length}개 더 있음</p> : null}</section>;
  };
  return <><p className="ha-summary-count">결정 {data.decisions.count} (기한 경과 {data.decisions.overdue}) · 작업 {data.tasks.count} · 배포 대기 {data.deploys.count}</p>{section("지금 할 일", action, data.decisions.count + data.tasks.count)}{section("참고", fyi, data.deploys.count)}{data.deploys.events_capped ? <p className="ha-note">조회 범위에 제한이 있어 목록이 불완전할 수 있습니다.</p> : null}</>;
}
export function HomeAttention() {
  const { data, failed } = usePoll(loadAttention, 60_000);
  return <div className="ha-root"><h1 className="ha-title">나를 기다리는 일</h1>{failed ? <p className="ha-error" role="alert">목록을 불러오지 못했습니다{data ? " · 마지막으로 불러온 데이터를 표시합니다" : ""}.</p> : null}{data ? <Body data={data} /> : failed ? null : <p role="status">불러오는 중…</p>}</div>;
}
