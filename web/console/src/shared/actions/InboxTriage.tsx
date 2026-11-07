// Inbox triage for Home and Work (hk 3343): the keyboard flow, the selection
// and its bulk bar, the snooze picker, the shortcut help and the Cmd-K
// palette, all built from the inbox action registry (./inbox). One page uses
// the hook once, so every page renders the same keys and the same words.

import { useEffect, useRef, useState } from "react";
import { destinations } from "../../app/Shell";
import { setTaskPriority } from "../boardApi";
import { inboxWriteMessage, undoInbox, writeInbox } from "../inboxApi";
import { isTypingTarget } from "../search";
import { formatMinuteKST } from "../time";
import { actions as taskActions, type ActionTarget } from "./index";
import { inboxActions, inboxActionLabel, snoozePresets, type InboxAction, type InboxActionId, type InboxTarget } from "./inbox";
import "./inbox.css";

/** One write covers at most this many items; a longer selection says so. */
export const BULK_LIMIT = 50;

export type TriageRow = InboxTarget;

type UndoEntry = { label: string; undo: () => Promise<void> };

export type Triage = ReturnType<typeof useInboxTriage>;

export type BulkActionState = { shown: boolean; disabled: boolean; reason: string | null };

/** How one registry action reads over a selection: hidden when it applies to
 * no row, enabled while one row can take it, and otherwise disabled with the
 * first Korean reason. */
export function actionState(action: InboxAction, targets: InboxTarget[]): BulkActionState {
  const applicable = targets.filter(target => action.applicable(target));
  if (applicable.length === 0) return { shown: false, disabled: true, reason: null };
  const usable = applicable.some(target => action.disabledReason(target) === null);
  const reason = applicable.map(target => action.disabledReason(target)).find((value): value is string => value !== null) ?? null;
  return { shown: true, disabled: !usable, reason };
}

/** The keyboard row index from the focused element, or -1 outside a row. */
function focusedRowIndex(rows: TriageRow[]): number {
  const element = document.activeElement;
  if (!(element instanceof Element)) return -1;
  const row = element.closest("[data-inbox-row]");
  const key = row instanceof HTMLElement ? row.dataset.inboxKey : undefined;
  return key ? rows.findIndex(candidate => candidate.key === key) : -1;
}

export function useInboxTriage(rows: TriageRow[], reload: () => void) {
  const rowsRef = useRef(rows);
  rowsRef.current = rows;
  const reloadRef = useRef(reload);
  reloadRef.current = reload;
  const stackRef = useRef<UndoEntry[]>([]);
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const selectedRef = useRef(selected);
  selectedRef.current = selected;
  const [anchor, setAnchor] = useState<string | null>(null);
  const [notice, setNotice] = useState("");
  const [undoCount, setUndoCount] = useState(0);
  const [snoozeOpen, setSnoozeOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [priorityOpen, setPriorityOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);

  const targetRows = (): TriageRow[] => {
    const list = rowsRef.current;
    const chosen = selectedRef.current;
    if (chosen.size > 0) return list.filter(row => chosen.has(row.key));
    // The focused row is read from the DOM, not from state: a mouse or Tab
    // focus has not re-rendered yet when the very next key arrives.
    const element = document.activeElement;
    const container = element instanceof Element ? element.closest("[data-inbox-row]") : null;
    const key = container instanceof HTMLElement ? container.dataset.inboxKey : undefined;
    return key ? list.filter(row => row.key === key) : [];
  };

  const pushUndo = (entry: UndoEntry) => {
    stackRef.current = [...stackRef.current.slice(-9), entry];
    setUndoCount(stackRef.current.length);
  };

  const runAction = async (id: InboxActionId, until?: string) => {
    if (busyRef.current) return;
    if (id === "snooze" && until === undefined) {
      setSnoozeOpen(true);
      return;
    }
    if (id === "priority") {
      setPriorityOpen(true);
      return;
    }
    if (id === "hold") {
      setNotice("보류는 작업 수명주기 콘솔 작업에서 제공합니다.");
      return;
    }
    const keys = targetRows().map(row => row.key);
    if (keys.length === 0) return;
    const capped = keys.slice(0, BULK_LIMIT);
    const eventKey = crypto.randomUUID();
    busyRef.current = true;
    setBusy(true);
    try {
      const result = await writeInbox(id, capped, eventKey, until);
      const parts = [`${inboxActionLabel(id)} ${result.items.length}건`];
      if (result.failed.length > 0) parts.push(`${result.failed.length}건 실패`);
      if (result.skipped > 0) parts.push(`${result.skipped}건 건너뜀`);
      if (keys.length > BULK_LIMIT) parts.push(`선택 ${keys.length}건 중 앞 ${BULK_LIMIT}건만 처리했습니다`);
      setNotice(parts.join(" · "));
      if (result.items.length > 0) {
        pushUndo({ label: inboxActionLabel(id), undo: async () => { await undoInbox(eventKey); } });
      }
      setSelected(new Set());
      setAnchor(null);
      reloadRef.current();
    } catch (error) {
      setNotice(inboxWriteMessage(error));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const applyPriority = async (value: number) => {
    setPriorityOpen(false);
    if (busyRef.current) return;
    const targets = targetRows()
      .map(row => row.task)
      .filter((task): task is ActionTarget => task !== undefined)
      .slice(0, BULK_LIMIT);
    if (targets.length === 0) {
      setNotice("우선순위를 바꿀 작업이 없습니다.");
      return;
    }
    busyRef.current = true;
    setBusy(true);
    try {
      const results = await Promise.all(targets.map(async task => ({ id: task.id, result: await setTaskPriority(task.id, value) })));
      pushUndo({
        label: "우선순위 변경",
        undo: async () => {
          for (const entry of results) await setTaskPriority(entry.id, entry.result.previous);
        },
      });
      setNotice(`우선순위 ${value === 0 ? "없음" : `P${value}`} · ${results.length}건`);
      setSelected(new Set());
      reloadRef.current();
    } catch {
      setNotice("우선순위를 변경하지 못했습니다.");
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const undo = async () => {
    const last = stackRef.current[stackRef.current.length - 1];
    if (!last) return;
    stackRef.current = stackRef.current.slice(0, -1);
    setUndoCount(stackRef.current.length);
    try {
      await last.undo();
      setNotice(`${last.label} 실행 취소`);
      reloadRef.current();
    } catch {
      setNotice("실행 취소하지 못했습니다.");
    }
  };

  const toggle = (key: string) => {
    setSelected(old => {
      const next = new Set(old);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
    setAnchor(key);
  };

  const rangeTo = (key: string) => {
    const list = rowsRef.current;
    const end = list.findIndex(row => row.key === key);
    if (end < 0) return;
    const start = list.findIndex(row => row.key === anchor);
    const from = start < 0 ? end : Math.min(start, end);
    const to = start < 0 ? end : Math.max(start, end);
    setSelected(old => {
      const next = new Set(old);
      for (let index = from; index <= to; index += 1) next.add(list[index].key);
      return next;
    });
  };

  const clearSelection = () => {
    setSelected(new Set());
    setAnchor(null);
  };

  const moveTo = (index: number) => {
    const list = rowsRef.current;
    if (list.length === 0) return;
    const next = Math.max(0, Math.min(list.length - 1, index));
    const key = list[next].key;
    for (const element of document.querySelectorAll<HTMLElement>("[data-inbox-row]")) {
      if (element.dataset.inboxKey === key) {
        element.focus();
        return;
      }
    }
  };

  const paletteEntries = () => {
    const targets = targetRows();
    const entries: { id: string; label: string; keywords: string[]; reason: string | null; run?: () => void }[] = [];
    if (targets.length > 0) {
      for (const action of inboxActions) {
        const state = actionState(action, targets);
        if (!state.shown) continue;
        entries.push({
          id: `inbox:${action.id}`,
          label: action.label,
          keywords: [action.id, ...action.keywords],
          reason: state.disabled ? state.reason : null,
          run: state.disabled ? undefined : () => { setPaletteOpen(false); void runAction(action.id); },
        });
      }
      const task = targets.length === 1 ? targets[0].task : undefined;
      if (task) {
        for (const action of taskActions) {
          if (!action.applicable(task)) continue;
          const reason = action.disabledReason(task);
          entries.push({
            id: `task:${action.id}`,
            label: action.label,
            keywords: [action.id, ...action.keywords],
            reason,
            run: reason ? undefined : () => { setPaletteOpen(false); setPriorityOpen(true); },
          });
        }
      }
    }
    for (const destination of destinations) {
      entries.push({
        id: `nav:${destination.key}`,
        label: `${destination.label} 화면으로 이동`,
        keywords: [destination.key, destination.label, destination.href],
        reason: null,
        run: () => { window.location.assign(destination.href); },
      });
    }
    return entries;
  };

  const handlers = useRef({ runAction, undo, moveTo, toggle, rangeTo, canUndo: false });
  handlers.current = { runAction, undo, moveTo, toggle, rangeTo, canUndo: undoCount > 0 };

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.isComposing) return;
      if ((event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === "k") {
        // Capture phase and stopPropagation: the page-level palette owns
        // Cmd-K, so a focused row's own palette never opens beside it.
        event.preventDefault();
        event.stopPropagation();
        setPaletteOpen(open => !open);
        return;
      }
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      if (snoozeOpen || helpOpen || paletteOpen || priorityOpen) return;
      if (isTypingTarget(event.target)) return;
      if (event.key === "?") {
        event.preventDefault();
        event.stopPropagation();
        setHelpOpen(true);
        return;
      }
      const index = focusedRowIndex(rowsRef.current);
      if (index < 0) return;
      const row = rowsRef.current[index];
      if (!row) return;
      const handler = handlers.current;
      switch (event.key) {
        case "j": handler.moveTo(index + 1); break;
        case "k": handler.moveTo(index - 1); break;
        case "e": void handler.runAction("done"); break;
        case "z":
          if (!handler.canUndo) return;
          void handler.undo();
          break;
        case "u": void handler.runAction(row.read ? "unread" : "read"); break;
        case "h": void handler.runAction("snooze"); break;
        case "x": if (event.shiftKey) handler.rangeTo(row.key); else handler.toggle(row.key); break;
        case "X": handler.rangeTo(row.key); break;
        default: return;
      }
      event.preventDefault();
      event.stopPropagation();
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [snoozeOpen, helpOpen, paletteOpen, priorityOpen]);

  return {
    selected,
    busy,
    notice,
    canUndo: undoCount > 0,
    snoozeOpen,
    helpOpen,
    paletteOpen,
    priorityOpen,
    setSnoozeOpen,
    setHelpOpen,
    setPaletteOpen,
    setPriorityOpen,
    selectedRows: () => rowsRef.current.filter(row => selected.has(row.key)),
    targetRows,
    runAction,
    applyPriority,
    undo,
    toggle,
    rangeTo,
    clearSelection,
    clearNotice: () => setNotice(""),
    paletteEntries,
  };
}

/** One page-level overlay set: the bulk bar, the toast and the three dialogs. */
export function TriageUi({ triage }: { triage: Triage }) {
  const selected = triage.selectedRows();
  const pickSnooze = (at: Date) => {
    triage.setSnoozeOpen(false);
    void triage.runAction("snooze", at.toISOString());
  };
  return (
    <>
      {selected.length > 0 ? (
        <div className="inbox-bar" role="region" aria-label="선택 항목 작업">
          <span className="inbox-bar-count">{selected.length}개 선택</span>
          {inboxActions.map(action => {
            const state = actionState(action, selected);
            if (!state.shown) return null;
            return (
              <button
                key={action.id}
                type="button"
                disabled={state.disabled || triage.busy}
                title={state.disabled ? state.reason ?? action.label : action.label}
                onClick={() => void triage.runAction(action.id)}
              >
                {action.label}
              </button>
            );
          })}
          <button type="button" className="inbox-bar-clear" onClick={triage.clearSelection}>
            선택 해제
          </button>
        </div>
      ) : null}
      {triage.notice ? (
        <div className="inbox-toast" role="status">
          <span>{triage.notice}</span>
          {triage.canUndo ? <button type="button" onClick={() => void triage.undo()}>실행 취소 (z)</button> : null}
          <button type="button" aria-label="알림 닫기" className="inbox-toast-close" onClick={triage.clearNotice}>×</button>
        </div>
      ) : null}
      {triage.snoozeOpen ? <SnoozePicker onPick={pickSnooze} close={() => triage.setSnoozeOpen(false)} /> : null}
      {triage.helpOpen ? <ShortcutHelp close={() => triage.setHelpOpen(false)} /> : null}
      {triage.priorityOpen ? <PriorityPicker onPick={value => void triage.applyPriority(value)} close={() => triage.setPriorityOpen(false)} /> : null}
      {triage.paletteOpen ? <InboxPalette triage={triage} /> : null}
    </>
  );
}

function SnoozePicker({ onPick, close }: { onPick: (at: Date) => void; close: () => void }) {
  const presets = snoozePresets(new Date());
  const [custom, setCustom] = useState("");
  const parsed = custom ? new Date(custom) : null;
  const valid = parsed !== null && Number.isFinite(parsed.getTime()) && parsed.getTime() > Date.now();
  return (
    <div className="inbox-backdrop" onClick={close}>
      <div role="dialog" aria-label="다시 알림 시각" className="inbox-dialog" onClick={event => event.stopPropagation()}>
        <h2>다시 알림</h2>
        <p>선택한 항목을 이 시각까지 숨깁니다. 시각이 되면 「다시 알림」으로 표시됩니다.</p>
        {presets.map(preset => (
          <button key={preset.id} type="button" onClick={() => onPick(preset.at)}>
            <span>{preset.label}</span>
            <span className="inbox-dialog-meta">{formatMinuteKST(preset.at.toISOString())}</span>
          </button>
        ))}
        <label className="inbox-dialog-row">
          <span>직접 선택</span>
          <input type="datetime-local" value={custom} onChange={event => setCustom(event.target.value)} />
        </label>
        <div className="inbox-dialog-row">
          <button type="button" disabled={!valid} onClick={() => valid && parsed && onPick(parsed)}>적용</button>
          <button type="button" className="inbox-bar-clear" onClick={close}>취소</button>
        </div>
      </div>
    </div>
  );
}

function PriorityPicker({ onPick, close }: { onPick: (value: number) => void; close: () => void }) {
  return (
    <div className="inbox-backdrop" onClick={close}>
      <div role="dialog" aria-label="우선순위 선택" className="inbox-dialog" onClick={event => event.stopPropagation()}>
        <h2>우선순위</h2>
        <p>선택한 작업의 우선순위를 한 번에 바꿉니다.</p>
        {[1, 2, 3, 4, 0].map(value => (
          <button key={value} type="button" onClick={() => onPick(value)}>
            {value === 0 ? "없음" : `P${value}`}
          </button>
        ))}
        <button type="button" className="inbox-bar-clear" onClick={close}>취소</button>
      </div>
    </div>
  );
}

const SHORTCUTS: { keys: string; label: string }[] = [
  { keys: "j / k", label: "다음 / 이전 항목으로 이동" },
  { keys: "Enter", label: "항목 열기" },
  { keys: "e", label: "완료" },
  { keys: "z", label: "실행 취소" },
  { keys: "u", label: "읽음 / 안 읽음" },
  { keys: "h", label: "다시 알림" },
  { keys: "x", label: "선택 / 선택 해제" },
  { keys: "Shift+x", label: "범위 선택" },
  { keys: "⌘K / Ctrl+K", label: "명령 팔레트" },
  { keys: "?", label: "단축키 도움말" },
];

function ShortcutHelp({ close }: { close: () => void }) {
  return (
    <div className="inbox-backdrop" onClick={close}>
      <div role="dialog" aria-label="단축키 도움말" className="inbox-dialog" onClick={event => event.stopPropagation()}>
        <h2>단축키</h2>
        <ul className="inbox-help">
          {SHORTCUTS.map(shortcut => (
            <li key={shortcut.keys}><kbd>{shortcut.keys}</kbd><span>{shortcut.label}</span></li>
          ))}
        </ul>
        <button type="button" className="inbox-bar-clear" onClick={close}>닫기</button>
      </div>
    </div>
  );
}

function InboxPalette({ triage }: { triage: Triage }) {
  const [query, setQuery] = useState("");
  const entries = triage.paletteEntries();
  const needle = query.trim().toLowerCase();
  const shown = entries.filter(entry => needle === "" || [entry.label, ...entry.keywords].some(word => word.toLowerCase().includes(needle)));
  return (
    <div className="inbox-backdrop" onClick={() => triage.setPaletteOpen(false)}>
      <div role="dialog" aria-label="명령 팔레트" className="inbox-dialog inbox-palette" onClick={event => event.stopPropagation()}>
        <label>명령 <input autoFocus value={query} onChange={event => setQuery(event.target.value)} placeholder="명령 또는 화면 검색" /></label>
        <ul>
          {shown.map(entry => (
            <li key={entry.id}>
              <button type="button" disabled={entry.run === undefined} title={entry.reason ?? ""} onClick={entry.run}>
                <span>{entry.label}</span>
                {entry.reason ? <span className="inbox-dialog-meta">{entry.reason}</span> : null}
              </button>
            </li>
          ))}
          {shown.length === 0 ? <li className="inbox-dialog-meta">일치하는 명령이 없습니다</li> : null}
        </ul>
      </div>
    </div>
  );
}
