import { useEffect, useRef, useState, type RefObject } from "react";
import { newEventKey, setTaskPriority } from "../boardApi";
import { isTypingTarget } from "../search";
import { actions, type Action, type ActionTarget } from "./index";
import { InstructDialog } from "./CommandLadder";
import type { OperatorCommandView } from "./commands";
import { UndoToast } from "./dialogs";
import { LifecycleAction } from "./LifecycleActions";
import "./actions.css";

export { ConfirmDialog, UndoToast } from "./dialogs";

type UndoEntry = { id: number; previous: number; current: number; onPriority: (priority: number) => void };
type LifecycleNote = { message: string; undo: () => Promise<void> };
type Running = { action: Action; key: string };

export function ActionNotices() {
  const [entry, setEntry] = useState<UndoEntry | null>(null);
  const [error, setError] = useState(false);
  const [note, setNote] = useState<LifecycleNote | null>(null);
  const [noteError, setNoteError] = useState<string | null>(null);
  useEffect(() => {
    // One undo at a time: the newest action owns the toast, so one "z" cannot
    // reverse two different writes.
    const success = (e: Event) => { if (e instanceof CustomEvent) { setError(false); setNote(null); setEntry(e.detail); } };
    const failed = () => { setEntry(null); setNote(null); setError(true); };
    const noteUndo = (e: Event) => { if (e instanceof CustomEvent) { setEntry(null); setNote(e.detail); } };
    const noteFailed = (e: Event) => { if (e instanceof CustomEvent) { setEntry(null); setNote(null); setNoteError(String(e.detail)); } };
    window.addEventListener("hk-priority-undo", success); window.addEventListener("hk-priority-error", failed);
    window.addEventListener("hk-lifecycle-undo", noteUndo); window.addEventListener("hk-lifecycle-error", noteFailed);
    return () => {
      window.removeEventListener("hk-priority-undo", success); window.removeEventListener("hk-priority-error", failed);
      window.removeEventListener("hk-lifecycle-undo", noteUndo); window.removeEventListener("hk-lifecycle-error", noteFailed);
    };
  }, []);
  useEffect(() => {
    if (!noteError) return;
    const timer = window.setTimeout(() => setNoteError(null), 10000);
    return () => window.clearTimeout(timer);
  }, [noteError]);
  const undo = () => { if (!entry) return; const saved = entry; setEntry(null); saved.onPriority(saved.previous); void setTaskPriority(saved.id, saved.previous).then(result => saved.onPriority(result.priority)).catch(() => { saved.onPriority(saved.current); setError(true); }); };
  const undoNote = () => { if (!note) return; const saved = note; setNote(null); void saved.undo().catch(() => setNoteError("실행 취소하지 못했습니다.")); };
  return <>
    {entry && <UndoToast undo={undo} close={() => setEntry(null)} />}
    {note && <UndoToast message={note.message} undo={undoNote} close={() => setNote(null)} />}
    {error && <div role="alert" className="action-toast action-toast-error">우선순위를 변경하지 못했습니다.</div>}
    {noteError && <div role="alert" className="action-toast action-toast-error">{noteError}</div>}
  </>;
}

export function TaskActions({ target, onPriority, onCommand, onApplied, compact = false }: { target: ActionTarget; onPriority: (priority: number) => void; onCommand?: (command: OperatorCommandView) => void; onApplied?: () => void; compact?: boolean }) {
  const [open, setOpen] = useState(false); const [palette, setPalette] = useState(false); const [query, setQuery] = useState("");
  const [menu, setMenu] = useState(false);
  const [running, setRunning] = useState<Running | null>(null);
  const [pending, setPending] = useState(false);
  const root = useRef<HTMLSpanElement>(null);
  const [instruct, setInstruct] = useState(false);
  const taskActions = actions.filter(a => (a.target ?? "task") === "task");
  const change = async (value: number) => {
    const before = target.priority; setPending(true); setOpen(false); setPalette(false); onPriority(value);
    try { const result = await setTaskPriority(target.id, value); onPriority(result.priority); window.dispatchEvent(new CustomEvent("hk-priority-undo", { detail: { id: target.id, previous: result.previous, current: result.priority, onPriority } })); }
    catch { onPriority(before); window.dispatchEvent(new Event("hk-priority-error")); } finally { setPending(false); }
  };
  const launch = (action: Action) => {
    setMenu(false); setPalette(false);
    if (action.disabledReason(target)) return;
    if (action.id === "priority") { setOpen(true); return; }
    if (action.id === "worker.message") { setInstruct(true); return; }
    setRunning({ action, key: newEventKey() });
  };
  const closeAll = () => { setOpen(false); setPalette(false); setMenu(false); };
  return <span ref={root} className="task-actions" onKeyDown={e => { if (e.key === "Escape") closeAll(); }}>
    <button className={compact ? "action-menu-button" : undefined} aria-haspopup="menu" aria-expanded={menu} aria-label={`작업 #${target.id} 메뉴`} title="작업 메뉴" disabled={pending} onClick={() => setMenu(!menu)}>{compact ? <span aria-hidden="true">⋯</span> : "작업 메뉴"}</button>
    {menu && <span className="action-menu" role="menu" aria-label="작업 메뉴">{taskActions.filter(a => a.applicable(target)).map(a => { const reason = a.disabledReason(target); return <button key={a.id} role="menuitem" disabled={!!reason} title={reason ?? a.label} onClick={() => launch(a)}>{a.label}{reason && <small>{reason}</small>}</button>; })}</span>}
    {open && <span className="priority-menu" role="menu" aria-label="우선순위 선택" onKeyDown={e => { if (!e.metaKey && !e.ctrlKey && !e.altKey && /^[0-4]$/.test(e.key)) { e.preventDefault(); void change(Number(e.key)); } }}><span className="priority-menu-heading">우선순위 선택</span>{[1,2,3,4,0].map((p, i) => <button key={p} autoFocus={i === 0} role="menuitemradio" aria-checked={target.priority === p} onClick={() => void change(p)}><span>{p === 0 ? "없음" : `P${p}`}{target.priority === p ? " · 현재" : ""}</span><kbd>{p}</kbd></button>)}</span>}
    {palette && <span className="action-palette" role="dialog" aria-label="명령 팔레트"><label>작업 #{target.id} 명령 <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder="명령 검색" /></label>{taskActions.filter(a => a.applicable(target) && [a.label, ...a.keywords].some(word => word.toLowerCase().includes(query.toLowerCase()))).map(a => <button key={a.id} disabled={!!a.disabledReason(target)} title={a.disabledReason(target) ?? ""} onClick={() => launch(a)}>{a.label}{a.shortcut ? ` · ${a.shortcut}` : ""}</button>)}</span>}
    {instruct && <InstructDialog taskId={target.id} close={() => setInstruct(false)} recorded={command => onCommand?.(command)} />}
    {running && <LifecycleAction action={running.action} target={target} eventKey={running.key} close={() => setRunning(null)} onApplied={() => onApplied?.()} />}
    <ActionKeys root={root} target={target} open={() => setOpen(true)} palette={() => setPalette(true)} />
  </span>;
}
function ActionKeys({ root, target, open, palette }: { root: RefObject<HTMLSpanElement>; target: ActionTarget; open: () => void; palette: () => void }) {
  useEffect(() => { const key = (e: KeyboardEvent) => { const el = document.activeElement; if (!(el instanceof Element) || !root.current?.closest("[data-action-task]")?.contains(el) || isTypingTarget(e.target)) return; const priority = actions[0]; if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); palette(); } else if (!e.metaKey && !e.ctrlKey && !e.altKey && e.key === "p" && !priority.disabledReason(target)) { e.preventDefault(); open(); } }; window.addEventListener("keydown", key); return () => window.removeEventListener("keydown", key); }, [root, target, open, palette]); return null;
}
