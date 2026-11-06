import { useEffect, useState } from "react";
import { setTaskPriority } from "../boardApi";
import { isTypingTarget } from "../search";
import { actions, type ActionTarget } from "./index";
import "./actions.css";

export function UndoToast({ undo, close }: { undo: () => void; close: () => void }) {
  useEffect(() => { window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: "우선순위를 변경했습니다. 실행 취소하려면 z를 누르세요." })); }, []);
  useEffect(() => { const timer = window.setTimeout(close, 10000); const key = (e: KeyboardEvent) => { if (e.key === "z" && !e.metaKey && !e.ctrlKey && !isTypingTarget(e.target)) { e.preventDefault(); undo(); } }; window.addEventListener("keydown", key); return () => { window.clearTimeout(timer); window.removeEventListener("keydown", key); }; }, [undo, close]);
  return <div className="action-toast">우선순위를 변경했습니다 <button onClick={undo}>실행 취소 (z)</button></div>;
}
export function ConfirmDialog({ title, effect, close, confirm }: { title: string; effect: string; close: () => void; confirm: () => void }) {
  return <dialog open aria-label={title} onKeyDown={e => { if (e.key === "Escape") { e.preventDefault(); close(); } if (e.key === "Tab") { const buttons = e.currentTarget.querySelectorAll<HTMLButtonElement>("button:not(:disabled)"); if (!buttons.length) return; if (e.shiftKey && document.activeElement === buttons[0]) { e.preventDefault(); buttons[buttons.length - 1].focus(); } else if (!e.shiftKey && document.activeElement === buttons[buttons.length - 1]) { e.preventDefault(); buttons[0].focus(); } } }}><h2>{title}</h2><button autoFocus onClick={close}>취소</button><button onClick={confirm}>{effect}</button></dialog>;
}
export function TaskActions({ target, onPriority }: { target: ActionTarget; onPriority: (priority: number) => void }) {
  const [open, setOpen] = useState(false); const [palette, setPalette] = useState(false); const [query, setQuery] = useState("");
  const [pending, setPending] = useState(false);
  const [undoValue, setUndoValue] = useState<number | null>(null); const [error, setError] = useState(false);
  useEffect(() => { const clear = () => setUndoValue(null); window.addEventListener("hk-action-undo-replace", clear); return () => window.removeEventListener("hk-action-undo-replace", clear); }, []);
  const change = async (value: number, undoing = false) => {
    const before = target.priority; setPending(true); setOpen(false); setPalette(false); setError(false); onPriority(value);
    try { const result = await setTaskPriority(target.id, value); onPriority(result.priority); window.dispatchEvent(new Event("hk-action-undo-replace")); setUndoValue(undoing ? null : result.previous); }
    catch { onPriority(before); setUndoValue(null); setError(true); } finally { setPending(false); }
  };
  const undo = () => { if (undoValue !== null && !pending) void change(undoValue, true); };
  return <span className="task-actions" onKeyDown={e => { if (e.key === "Escape") { setOpen(false); setPalette(false); } }}>
    {actions.filter(a => a.applicable(target)).map(a => <button key={a.id} disabled={pending || !!a.disabledReason(target)} title={a.disabledReason(target) ?? a.label} onClick={() => setOpen(!open)}>{a.label} ({a.shortcut})</button>)}
    {open && <span className="priority-menu" role="group" aria-label="우선순위 선택">{[0,1,2,3,4,5].map(p => <button key={p} onClick={() => void change(p)}>{p === 0 ? "미지정" : `P${p}`}</button>)}</span>}
    {palette && <span className="action-palette" role="dialog" aria-label="명령 팔레트"><label>작업 #{target.id} 명령 <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder="명령 검색" /></label>{actions.filter(a => a.applicable(target) && [a.label, ...a.keywords].some(word => word.toLowerCase().includes(query.toLowerCase()))).map(a => <button key={a.id} disabled={!!a.disabledReason(target)} title={a.disabledReason(target) ?? ""} onClick={() => { setPalette(false); setQuery(""); setOpen(true); }}>{a.label} · {a.shortcut}</button>)}</span>}
    <ActionKeys target={target} open={() => setOpen(true)} palette={() => setPalette(true)} />
    {undoValue !== null && <UndoToast undo={undo} close={() => setUndoValue(null)} />}{error && <span role="alert" className="action-toast">우선순위를 변경하지 못했습니다.</span>}
  </span>;
}
function ActionKeys({ target, open, palette }: { target: ActionTarget; open: () => void; palette: () => void }) {
  useEffect(() => { const key = (e: KeyboardEvent) => { const el = document.activeElement; if (!(el instanceof Element) || !el.closest(`[data-action-task="${target.id}"]`) || isTypingTarget(e.target)) return; if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); palette(); } else if (!e.metaKey && !e.ctrlKey && !e.altKey && e.key === "p" && !actions[0].disabledReason(target)) { e.preventDefault(); open(); } }; window.addEventListener("keydown", key); return () => window.removeEventListener("keydown", key); }, [target, open, palette]); return null;
}
