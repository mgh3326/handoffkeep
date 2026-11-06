import { useCallback, useEffect, useState } from "react";
import { readStored, writeStored } from "../shared/storage";
import { usePoll } from "../shared/usePoll";
import { loadList } from "./api";
import { DecisionPage } from "./DecisionPage";
import { ResultBadge } from "./ResultBadge";
import type { DecisionRecord, Tab } from "./types";
import "./decisions.css";
const tabs: { id: Tab; label: string }[] = [
  { id: "open", label: "열림" },
  { id: "overdue", label: "기한 경과" },
  { id: "answered", label: "답변 기록" },
  { id: "cleanup", label: "정리 필요" },
];
const seenKey = "hk-console:decisions-seen";
function initialTab(): Tab {
  const tab = new URLSearchParams(location.search).get("tab");
  return tabs.find((x) => x.id === tab)?.id ?? "open";
}
function readSeen(): Record<string, number> {
  try {
    const v: unknown = JSON.parse(readStored(seenKey) ?? "{}");
    if (
      typeof v === "object" &&
      v !== null &&
      !Array.isArray(v) &&
      Object.values(v).every((x) => typeof x === "number")
    )
      return v as Record<string, number>;
  } catch {
    /* no saved set */
  }
  return {};
}
function itemState(x: DecisionRecord): string {
  return (
    x.request_state || (x.status === "open" && x.overdue ? "overdue" : x.status)
  );
}
const labels: Record<string, string> = {
  open: "열림 · 응답 대기",
  overdue: "기한 경과 · 기본값 미적용",
  uncleaned: "미정리 요청",
  answered: "답변됨",
  default_applied: "기본값 적용됨",
  withdrawn: "철회됨",
  superseded: "대체됨",
  unrecorded: "미기록",
};
function stateLabel(raw: string): string {
  return Object.prototype.hasOwnProperty.call(labels, raw)
    ? labels[raw]
    : `지원되지 않는 상태 ${raw}`;
}
export function DecisionList() {
  const [tab, setTab] = useState<Tab>(initialTab);
  const [selected, setSelected] = useState<string | null>(() =>
    readStored("hk-decisions:selected", sessionStorage),
  );
  const [wide, setWide] = useState(
    () => matchMedia("(min-width: 1024px)").matches,
  );
  const [seen, setSeen] = useState(readSeen);
  const [ordered, setOrdered] = useState<DecisionRecord[]>([]);
  const load = useCallback(() => loadList(tab), [tab]);
  const { data, failed, at } = usePoll(load, 60_000);
  useEffect(() => {
    const saved = readStored("hk-decisions:scroll", sessionStorage);
    if (saved !== null && Number.isFinite(Number(saved)))
      window.scrollTo(0, Number(saved));
  }, []);
  useEffect(() => {
    const m = matchMedia("(min-width: 1024px)");
    const change = () => setWide(m.matches);
    m.addEventListener("change", change);
    return () => m.removeEventListener("change", change);
  }, []);
  useEffect(() => {
    if (!data || data.tab !== tab) return;
    setOrdered((previous) => {
      const fresh = new Map(data.items.map((item) => [item.id, item]));
      const retained = previous.flatMap((item) => {
        const updated = fresh.get(item.id);
        fresh.delete(item.id);
        return updated ? [updated] : [];
      });
      return [...retained, ...fresh.values()];
    });
  }, [data, tab]);
  useEffect(() => {
    const back = () => {
      setTab(initialTab());
      const match = /^\/ui\/decisions\/([1-9][0-9]*)$/.exec(location.pathname);
      setSelected(
        match?.[1] ?? readStored("hk-decisions:selected", sessionStorage),
      );
      const y = Number(readStored("hk-decisions:scroll", sessionStorage));
      if (Number.isFinite(y)) window.scrollTo(0, y);
    };
    window.addEventListener("popstate", back);
    return () => window.removeEventListener("popstate", back);
  }, []);
  const chooseTab = (t: Tab) => {
    setTab(t);
    setOrdered([]);
    history.pushState(null, "", `/ui/decisions?tab=${t}`);
  };
  const choose = (id: number, event: React.MouseEvent<HTMLAnchorElement>) => {
    writeStored("hk-decisions:scroll", String(window.scrollY), sessionStorage);
    writeStored("hk-decisions:selected", String(id), sessionStorage);
    if (
      wide &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.shiftKey &&
      !event.altKey
    ) {
      event.preventDefault();
      setSelected(String(id));
      history.pushState(null, "", `/ui/decisions/${id}?tab=${tab}`);
    }
  };
  const markSeen = () => {
    const next = { ...seen };
    ordered.forEach((x) => {
      next[String(x.id)] = x.revision;
    });
    setSeen(next);
    writeStored(seenKey, JSON.stringify(next));
  };
  let divider = false;
  return (
    <div className="decision-list-layout">
      <section className="decision-list">
        <h1>결정</h1>
        <nav aria-label="결정 탭" className="decision-tabs">
          {tabs.map((x) => (
            <button
              key={x.id}
              aria-current={tab === x.id ? "page" : undefined}
              onClick={() => chooseTab(x.id)}
            >
              {x.label}
            </button>
          ))}
        </nav>
        <button onClick={markSeen}>확인함</button>
        {failed && (!data || data.tab !== tab) && (
          <p role="alert">불러오지 못함</p>
        )}
        {failed && data?.tab === tab && (
          <p>
            마지막 확인 {at ? new Date(at).toLocaleString() : data.generated_at}
          </p>
        )}
        {data?.tab === tab && data.truncated && (
          <p>{ordered.length}건만 로드</p>
        )}
        {data?.tab === tab && !data.truncated && !ordered.length && <p>0건</p>}
        {ordered.map((x) => {
          const isNew = seen[String(x.id)] === undefined;
          const firstNew = isNew && !divider;
          if (firstNew) divider = true;
          return (
            <div key={x.id}>
              {firstNew && <p>새 항목</p>}
              <a
                href={`/ui/decisions/${x.id}?tab=${tab}`}
                className="decision-row"
                aria-current={selected === String(x.id) ? "true" : undefined}
                onClick={(event) => choose(x.id, event)}
              >
                <strong>
                  #{x.id} {x.question}
                </strong>
                <span>
                  {x.lane} · {stateLabel(itemState(x))}
                </span>
                {seen[String(x.id)] !== undefined &&
                  seen[String(x.id)] !== x.revision && <span>새 revision</span>}
                {itemState(x) === "answered" && (
                  <ResultBadge result="answered" />
                )}
              </a>
              {x.superseded_record_id && (
                <a href={`/ui/decisions/${x.superseded_record_id}`}>
                  대체 요청 {x.superseded_by}
                </a>
              )}
            </div>
          );
        })}
      </section>
      {wide && selected && (
        <aside className="decision-panel">
          <DecisionPage key={selected} id={selected} />
        </aside>
      )}
    </div>
  );
}
