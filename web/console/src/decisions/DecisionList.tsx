import { useShowOriginal } from "../shared/translation";
import { useCallback, useEffect, useState } from "react";
import { Icon } from "../shared/Icon";
import { StateIcon } from "../shared/StateIcon";
import { readStored, writeStored } from "../shared/storage";
import { formatMinuteKST } from "../shared/time";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { usePoll } from "../shared/usePoll";
import { loadList } from "./api";
import { DecisionChip, sourceLabel, stateChip, stateShape } from "./DecisionChip";
import { DecisionPage } from "./DecisionPage";
import { formatAge, formatRemaining, summarizeQuestion } from "./format";
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
/** One line of guidance per tab, and where to go next. */
const empty: Record<Tab, { text: string; next: Tab; nextLabel: string }> = {
  open: { text: "답할 결정이 없어요", next: "answered", nextLabel: "답변 기록 보기" },
  overdue: { text: "기한이 지난 결정이 없어요", next: "open", nextLabel: "열린 결정 보기" },
  answered: { text: "아직 답변한 결정이 없어요", next: "open", nextLabel: "열린 결정 보기" },
  cleanup: { text: "정리할 요청이 없어요", next: "open", nextLabel: "열린 결정 보기" },
};
const plainClick = (e: React.MouseEvent) =>
  !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey && e.button === 0;
export function DecisionList() {
  const showOriginal = useShowOriginal();
  const [originalItems, setOriginalItems] = useState<Record<number, boolean>>({});
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
  const now = data?.generated_at ?? "";
  const counts = data?.counts;
  const loaded = data?.tab === tab;
  return (
    <div className="decision-list-layout">
      <section className="decision-list">
        <h1>결정</h1>
        <div className="decision-toolbar">
          <nav aria-label="결정 탭" className="decision-tabs">
            {tabs.map((x) => {
              const count = x.id === "open" || x.id === "overdue" ? counts?.[x.id] : undefined;
              return (
                <a
                  key={x.id}
                  href={`/ui/decisions?tab=${x.id}`}
                  className="decision-tab"
                  data-tab={x.id}
                  aria-current={tab === x.id ? "page" : undefined}
                  onClick={(event) => {
                    if (!plainClick(event)) return;
                    event.preventDefault();
                    chooseTab(x.id);
                  }}
                >
                  {x.label}
                  {count !== undefined && (
                    <span className="decision-tab-count" data-zero={count === 0 ? "true" : undefined}>
                      {count}
                    </span>
                  )}
                </a>
              );
            })}
          </nav>
          <button type="button" className="hk-button" onClick={markSeen}>
            모두 확인
          </button>
        </div>
        {failed && !loaded && (
          <p role="alert" className="decision-note">
            불러오지 못함
          </p>
        )}
        {failed && loaded && (
          <p className="decision-note">
            마지막 확인 {at ? new Date(at).toLocaleString() : data.generated_at}
          </p>
        )}
        {loaded && data.truncated && (
          <p className="decision-note">{ordered.length}건만 로드</p>
        )}
        {loaded && !data.truncated && !ordered.length && (
          <div className="decision-empty">
            <Icon name="check" className="decision-empty-icon" />
            <p className="decision-empty-text">{empty[tab].text}</p>
            <a
              href={`/ui/decisions?tab=${empty[tab].next}`}
              onClick={(event) => {
                if (!plainClick(event)) return;
                event.preventDefault();
                chooseTab(empty[tab].next);
              }}
            >
              {empty[tab].nextLabel}
            </a>
          </div>
        )}
        {ordered.map((x) => {
          const isNew = seen[String(x.id)] === undefined;
          const firstNew = isNew && !divider;
          if (firstNew) divider = true;
          const state = itemState(x);
          const age = formatAge(now, x.created_at);
          const remaining = state === "open" && x.due_at ? formatRemaining(now, x.due_at) : null;
          return (
            <div className="decision-translation-row" key={x.id}>
              {firstNew && <p className="decision-divider">새 항목</p>}
              <a
                href={`/ui/decisions/${x.id}?tab=${tab}`}
                className={`decision-row${isNew ? " decision-row--new" : ""}`}
                aria-current={selected === String(x.id) ? "true" : undefined}
                onClick={(event) => choose(x.id, event)}
              >
                <span className="decision-row-icon">
                  <StateIcon state={stateShape(state)} />
                </span>
                <span className="decision-row-main">
                  <span className="decision-row-title">
                    {isNew && <span className="hk-sr-only">안 읽음 </span>}
                    <Mono className="decision-id">#{x.id}</Mono>{" "}
                    <span className="decision-summary">{summarizeQuestion(!(originalItems[x.id] ?? showOriginal) && x.question_ko && !x.question_ko_stale ? x.question_ko : x.question)}{x.question_ko_stale && <small> 갱신 필요</small>}</span>
                  </span>
                  <span className="hk-chips">
                    <span className="hk-chip hk-chip--neutral" data-chip="source">
                      {sourceLabel(x.source)}
                    </span>
                    {state !== "open" && <DecisionChip {...stateChip(state)} />}
                    {remaining && (
                      <DecisionChip group="input" label={`기한 ${remaining} 남음`} />
                    )}
                    {x.lane && <Chip kind="lane" lane={x.lane} />}
                    {seen[String(x.id)] !== undefined && seen[String(x.id)] !== x.revision && (
                      <DecisionChip group="input" label="새 revision" />
                    )}
                    {age && (
                      <time
                        className="decision-age"
                        dateTime={x.created_at}
                        title={formatMinuteKST(x.created_at) ?? undefined}
                      >
                        {age}
                      </time>
                    )}
                  </span>
                </span>
              </a>
              {x.question_ko && !x.question_ko_stale && <button type="button" aria-pressed={originalItems[x.id] ?? showOriginal} onClick={() => setOriginalItems(items => ({...items, [x.id]: !(items[x.id] ?? showOriginal)}))}>{(originalItems[x.id] ?? showOriginal) ? "번역" : "원문"}</button>}
              {x.superseded_record_id && (
                <a className="decision-superseded" href={`/ui/decisions/${x.superseded_record_id}`}>
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
