import { useEffect, useMemo, useState } from "react";
import { fetchBoardTasks, fetchPolicyActive, type BoardData } from "./api";
import type { BoardTask, PolicyResponse } from "./types";
import { TaskDetail } from "../task-detail/TaskDetail";
import { PolicyPanel } from "./PolicyPanel";

const FALLBACK_STATES = [
  "backlog",
  "claimed",
  "in_progress",
  "verifying",
  "join",
  "hold",
  "needs_decision",
  "merged",
  "dropped",
];

const POLL_MS = 15_000;

export function taskOrder(a: BoardTask, b: BoardTask): number {
  if (a.priority !== b.priority) {
    return b.priority - a.priority;
  }
  if (a.created_at !== b.created_at) {
    return a.created_at < b.created_at ? -1 : 1;
  }
  return a.id - b.id;
}

// The operator preset mirrors the retired htmx queue view: backlog collapses
// to a count, and every other column only shows work that needs an operator.
export function operatorVisible(task: BoardTask): boolean {
  if (task.state === "backlog") {
    return true;
  }
  return task.state === "needs_decision" || (task.kind === "decide" && task.state !== "merged" && task.state !== "dropped");
}

type Filters = {
  lane: string;
  kind: string;
  query: string;
  hiddenStates: Set<string>;
  operatorOnly: boolean;
};

const EMPTY_FILTERS: Filters = { lane: "", kind: "", query: "", hiddenStates: new Set(), operatorOnly: false };

function TaskCard({ task, selected, onSelect }: { task: BoardTask; selected: boolean; onSelect: (id: number) => void }) {
  return (
    <button type="button" className={`board-card${selected ? " selected" : ""}${task.state === "needs_decision" ? " decision-card" : ""}`} onClick={() => onSelect(task.id)}>
      <span className="board-card-title">
        #{task.id} {task.title}
      </span>
      <span className="board-card-meta">
        {task.lane} · {task.kind} · p{task.priority}
        {task.claimed_by ? ` · ${task.claimed_by}` : ""}
      </span>
    </button>
  );
}

function BoardColumn({ state, tasks, collapsed, selectedId, onSelect }: { state: string; tasks: BoardTask[]; collapsed: boolean; selectedId: number | null; onSelect: (id: number) => void }) {
  return (
    <section className="board-column" data-state={state}>
      <h3>
        {state} <span className="badge">{tasks.length}</span>
      </h3>
      {collapsed ? (
        <p className="muted">접힘 ({tasks.length})</p>
      ) : tasks.length === 0 ? (
        <p className="muted">없음</p>
      ) : (
        tasks.map((task) => <TaskCard key={task.id} task={task} selected={task.id === selectedId} onSelect={onSelect} />)
      )}
    </section>
  );
}

export function BoardApp() {
  const [board, setBoard] = useState<BoardData | null>(null);
  const [boardError, setBoardError] = useState(false);
  const [firstLoad, setFirstLoad] = useState(true);
  const [policy, setPolicy] = useState<PolicyResponse | null>(null);
  const [policyError, setPolicyError] = useState(false);
  const [filters, setFilters] = useState<Filters>(EMPTY_FILTERS);
  const [selectedId, setSelectedId] = useState<number | null>(null);

  // The next refresh is scheduled only after the in-flight load settles, so
  // polls can never overlap. The sequence guard keeps a stale response from
  // overwriting newer state if ordering ever changes.
  useEffect(() => {
    let cancelled = false;
    let timer = 0;
    let seq = 0;
    const load = async () => {
      const mine = ++seq;
      try {
        const next = await fetchBoardTasks();
        if (!cancelled && mine === seq) {
          setBoard(next);
          setBoardError(false);
        }
      } catch {
        if (!cancelled && mine === seq) {
          setBoardError(true);
        }
      } finally {
        if (!cancelled) {
          setFirstLoad(false);
          timer = window.setTimeout(() => {
            void load();
          }, POLL_MS);
        }
      }
    };
    void load();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    fetchPolicyActive()
      .then((next) => {
        if (!cancelled) {
          setPolicy(next);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setPolicyError(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const lanes = useMemo(() => {
    const seen = new Set<string>();
    for (const task of board?.tasks ?? []) {
      seen.add(task.lane);
    }
    return [...seen].sort();
  }, [board]);

  const kinds = useMemo(() => {
    const seen = new Set<string>();
    for (const task of board?.tasks ?? []) {
      seen.add(task.kind);
    }
    return [...seen].sort();
  }, [board]);

  const states = board?.states?.length ? board.states : FALLBACK_STATES;

  const columns = useMemo(() => {
    const query = filters.query.trim();
    const grouped = new Map<string, BoardTask[]>();
    for (const state of states) {
      grouped.set(state, []);
    }
    for (const task of board?.tasks ?? []) {
      if (filters.lane !== "" && task.lane !== filters.lane) {
        continue;
      }
      if (filters.kind !== "" && task.kind !== filters.kind) {
        continue;
      }
      if (filters.hiddenStates.has(task.state)) {
        continue;
      }
      if (query !== "" && !task.title.includes(query) && String(task.id) !== query) {
        continue;
      }
      if (filters.operatorOnly && !operatorVisible(task)) {
        continue;
      }
      const list = grouped.get(task.state);
      if (list) {
        list.push(task);
      }
    }
    for (const list of grouped.values()) {
      list.sort(taskOrder);
    }
    return grouped;
  }, [board, filters, states]);

  if (firstLoad && board === null) {
    return <p className="muted">불러오는 중</p>;
  }
  if (board === null) {
    return <p className="muted">보드를 불러오지 못했습니다.</p>;
  }

  const toggleState = (state: string) => {
    const hidden = new Set(filters.hiddenStates);
    if (hidden.has(state)) {
      hidden.delete(state);
    } else {
      hidden.add(state);
    }
    setFilters({ ...filters, hiddenStates: hidden });
  };

  return (
    <section className="board">
      <h2>Queue board</h2>
      {boardError ? <p className="badge board-warning">갱신 실패 — 마지막으로 받은 데이터를 표시합니다.</p> : null}
      {board.truncated ? <p className="badge board-warning">일부만 표시됨 — 서버/클라이언트 한계에 도달했습니다.</p> : null}
      <form className="board-filters" onSubmit={(event) => event.preventDefault()}>
        <label htmlFor="board-filter-lane">Lane</label>
        <select id="board-filter-lane" value={filters.lane} onChange={(event) => setFilters({ ...filters, lane: event.target.value })}>
          <option value="">전체</option>
          {lanes.map((lane) => (
            <option key={lane} value={lane}>
              {lane}
            </option>
          ))}
        </select>
        <label htmlFor="board-filter-kind">Kind</label>
        <select id="board-filter-kind" value={filters.kind} onChange={(event) => setFilters({ ...filters, kind: event.target.value })}>
          <option value="">전체</option>
          {kinds.map((kind) => (
            <option key={kind} value={kind}>
              {kind}
            </option>
          ))}
        </select>
        <label htmlFor="board-filter-query">검색</label>
        <input id="board-filter-query" value={filters.query} onChange={(event) => setFilters({ ...filters, query: event.target.value })} />
        <fieldset className="board-state-filter">
          <legend>상태</legend>
          {states.map((state) => (
            <label key={state}>
              <input type="checkbox" checked={!filters.hiddenStates.has(state)} onChange={() => toggleState(state)} /> {state}
            </label>
          ))}
        </fieldset>
        <label>
          <input type="checkbox" checked={filters.operatorOnly} onChange={(event) => setFilters({ ...filters, operatorOnly: event.target.checked })} /> 운영자 필요만
        </label>
        <button type="button" onClick={() => setFilters(EMPTY_FILTERS)}>
          필터 초기화
        </button>
      </form>
      <div className="board-columns">
        {states.map((state) => {
          const tasks = columns.get(state) ?? [];
          return <BoardColumn key={state} state={state} tasks={tasks} collapsed={filters.operatorOnly && state === "backlog"} selectedId={selectedId} onSelect={setSelectedId} />;
        })}
      </div>
      {board.tasks.length === 0 ? <p className="muted">표시할 태스크가 없습니다.</p> : null}
      {selectedId !== null ? <TaskDetail id={selectedId} onClose={() => setSelectedId(null)} /> : null}
      <PolicyPanel policy={policy} error={policyError} />
    </section>
  );
}
