import { STORAGE_KEYS, writeStored } from "../shared/storage";
import { useCallback, useEffect, useRef, useState } from "react";
import { fetchBoardTasks, fetchLive } from "../shared/boardApi";
import { usePoll } from "../shared/usePoll";
import { liveRequestFailed } from "../live";
import { ExecutionChips } from "./ExecutionChips";
import { TaskPage } from "./TaskPage";
import { PageShell } from "../shared/AppShell";
import { DeploySummary } from "./DeploySummary";
import { stateLabel } from "../shared/states";

async function load() {
  const [board, live] = await Promise.all([
    fetchBoardTasks(),
    fetchLive().catch(liveRequestFailed),
  ]);
  return { board, live };
}
export function ConsoleBoard() {
  const { data, failed } = usePoll(load, 15000);
  const query = new URLSearchParams(window.location.search);
  const [lane, setLane] = useState(query.get("lane") ?? "");
  const [claimant, setClaimant] = useState(query.get("claimed_by") ?? "");
  const [state, setState] = useState(query.get("state") ?? "");
  const [text, setText] = useState(query.get("q") ?? "");
  const [mine, setMine] = useState(query.get("mine") === "1");
  const [selected, setSelected] = useState<number | null>(
    Number(query.get("task")) || null,
  );
  const opener = useRef<HTMLElement | null>(null);
  const scroll = useRef(0);
  useEffect(() => {
    if (lane || claimant) {
      const scope = new URLSearchParams();
      if (lane) scope.set("lane", lane);
      if (claimant) scope.set("claimed_by", claimant);
      writeStored(STORAGE_KEYS.scope, scope.toString());
    }
    const p = new URLSearchParams(window.location.search);
    for (const [key, value] of Object.entries({
      lane,
      claimed_by: claimant,
      state,
      q: text,
      mine: mine ? "1" : "",
    })) {
      if (value) p.set(key, value);
      else p.delete(key);
    }
    window.history.replaceState(null, "", `?${p}`);
  }, [lane, claimant, state, text, mine]);
  useEffect(() => {
    const pop = () =>
      setSelected(
        Number(new URLSearchParams(window.location.search).get("task")) || null,
      );
    window.addEventListener("popstate", pop);
    return () => window.removeEventListener("popstate", pop);
  }, []);
  const close = useCallback(() => {
    setSelected(null);
    const p = new URLSearchParams(window.location.search);
    p.delete("task");
    window.history.pushState(null, "", `?${p}`);
    requestAnimationFrame(() => {
      opener.current?.focus();
      window.scrollTo(0, scroll.current);
    });
  }, []);
  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !document.querySelector("dialog[open]"))
        close();
    };
    if (selected) window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [selected, close]);
  const tasks =
    data?.board.tasks.filter(
      (t) =>
        (!state || t.state === state) &&
        (!lane || t.lane === lane) &&
        (!claimant || t.claimed_by === claimant) &&
        (!mine || lane || claimant) &&
        `${t.title} #${t.id}`.toLowerCase().includes(text.toLowerCase()),
    ) ?? [];
  return (
    <PageShell current="queue">
      <div className="hk-view">
        <div className="hk-title">
          <h1>{mine ? "My work" : "All work"}</h1>
          <p>
            {data
              ? `${tasks.length}${data.board.truncated ? " loaded · incomplete" : " tasks"}`
              : "Loading…"}
          </p>
        </div>
        {failed ? (
          <p role="alert">
            {data
              ? "Refresh failed · showing last good snapshot"
              : "Board unavailable · retrying"}
          </p>
        ) : null}
        {selected ? (
          <>
            <button id="board-back" onClick={close}>
              ← Back to board · Esc
            </button>
            <TaskPage id={selected} />
          </>
        ) : null}
        <div hidden={selected !== null}>
          <div className="hk-filters">
            <label>
              Title / task ID
              <input value={text} onChange={(e) => setText(e.target.value)} />
            </label>
            <label>
              State
              <select value={state} onChange={(e) => setState(e.target.value)}>
                <option value="">All states</option>
                {data?.board.states.map((s) => (
                  <option key={s} value={s}>
                    {stateLabel(s)}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Lane
              <input value={lane} onChange={(e) => setLane(e.target.value)} />
            </label>
            <label>
              Claimant
              <input
                value={claimant}
                onChange={(e) => setClaimant(e.target.value)}
              />
            </label>
            <button aria-pressed={mine} onClick={() => setMine(!mine)}>
              My work
            </button>
          </div>
          {mine && !lane && !claimant ? (
            <p>
              Select a lane or claimant to define My work. Identity is never
              inferred from email.
            </p>
          ) : null}
          <DeploySummary />
          {data ? (
            <p className="muted">
              As of {data.board.generatedAt} · live jobs {data.live.jobs.status}{" "}
              · {data.live.jobs.fetched_at || "Unavailable"}
            </p>
          ) : null}
          {[
            ...new Set([
              ...(data?.board.states ?? []),
              ...tasks.map((t) => t.state),
            ]),
          ].map((s) => {
            const rows = tasks
              .filter((t) => t.state === s)
              .sort((a, b) => b.priority - a.priority || a.id - b.id);
            return rows.length ? (
              <section className="hk-group" key={s}>
                <h2>
                  {stateLabel(s)} <small>{rows.length}</small>
                </h2>
                {rows.map((t) => {
                  return (
                    <article
                      data-console-row
                      className="hk-task-row"
                      tabIndex={0}
                      key={t.id}
                    >
                      <span className="muted">
                        hk {t.id}
                        <br />p{t.priority}
                      </span>
                      <a
                        href={`/ui/tasks/${t.id}`}
                        onClick={(e) => {
                          if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey)
                            return;
                          e.preventDefault();
                          opener.current = e.currentTarget;
                          scroll.current = window.scrollY;
                          setSelected(t.id);
                          requestAnimationFrame(() =>
                            document.getElementById("board-back")?.focus(),
                          );
                          const p = new URLSearchParams(window.location.search);
                          p.set("task", String(t.id));
                          window.history.pushState(null, "", `?${p}`);
                        }}
                      >
                        <strong>{t.title}</strong>
                      </a>
                      <span className="hk-row-meta">
                        {data ? <ExecutionChips task={t} live={data.live} /> : null}
                        <span>
                          {t.lane} · {t.claimed_by || "Unclaimed"}
                        </span>
                      </span>
                    </article>
                  );
                })}
              </section>
            ) : null;
          })}
          {data && !tasks.length && (!mine || lane || claimant) ? (
            <p>No tasks match these filters.</p>
          ) : null}
        </div>
      </div>
    </PageShell>
  );
}
