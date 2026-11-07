import { useEffect, useRef, useState, type RefObject } from "react";
import { setTaskPriority } from "../boardApi";
import { isTypingTarget } from "../search";
import { actions, type ActionTarget } from "./index";
import "./actions.css";

export function UndoToast({ undo, close }: { undo: () => void; close: () => void }) {
  useEffect(() => { window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: "우선순위를 변경했습니다. 실행 취소하려면 z를 누르세요." })); }, []);
  useEffect(() => { const timer = window.setTimeout(close, 10000); const key = (e: KeyboardEvent) => { if (e.key === "z" && !e.metaKey && !e.ctrlKey && !isTypingTarget(e.target)) { e.preventDefault(); undo(); } }; window.addEventListener("keydown", key); return () => { window.clearTimeout(timer); window.removeEventListener("keydown", key); }; }, [undo, close]);
  return <div className="action-toast">우선순위를 변경했습니다 <button onClick={undo}>실행 취소 (z)</button></div>;
}
type UndoEntry = { id: number; previous: number; current: number; onPriority: (priority: number) => void };
export function ActionNotices() {
  const [entry, setEntry] = useState<UndoEntry | null>(null);
  const [error, setError] = useState(false);
  useEffect(() => {
    const success = (e: Event) => { if (e instanceof CustomEvent) { setError(false); setEntry(e.detail); } };
    const failed = () => { setEntry(null); setError(true); };
    window.addEventListener("hk-priority-undo", success); window.addEventListener("hk-priority-error", failed);
    return () => { window.removeEventListener("hk-priority-undo", success); window.removeEventListener("hk-priority-error", failed); };
  }, []);
  const undo = () => { if (!entry) return; const saved = entry; setEntry(null); saved.onPriority(saved.previous); void setTaskPriority(saved.id, saved.previous).then(result => saved.onPriority(result.priority)).catch(() => { saved.onPriority(saved.current); setError(true); }); };
  return <>{entry && <UndoToast undo={undo} close={() => setEntry(null)} />}{error && <div role="alert" className="action-toast">우선순위를 변경하지 못했습니다.</div>}</>;
}
export function ConfirmDialog({ title, effect, close, confirm }: { title: string; effect: string; close: () => void; confirm: () => void }) {
  return <dialog open aria-label={title} onKeyDown={e => { if (e.key === "Escape") { e.preventDefault(); close(); } if (e.key === "Tab") { const buttons = e.currentTarget.querySelectorAll<HTMLButtonElement>("button:not(:disabled)"); if (!buttons.length) return; if (e.shiftKey && document.activeElement === buttons[0]) { e.preventDefault(); buttons[buttons.length - 1].focus(); } else if (!e.shiftKey && document.activeElement === buttons[buttons.length - 1]) { e.preventDefault(); buttons[0].focus(); } } }}><h2>{title}</h2><button autoFocus onClick={close}>취소</button><button onClick={confirm}>{effect}</button></dialog>;
}
export function TaskActions({ target, onPriority, compact = false }: { target: ActionTarget; onPriority: (priority: number) => void; compact?: boolean }) {
  const [open, setOpen] = useState(false); const [palette, setPalette] = useState(false); const [query, setQuery] = useState("");
  const [pending, setPending] = useState(false);
  const root = useRef<HTMLSpanElement>(null);
  const change = async (value: number) => {
    const before = target.priority; setPending(true); setOpen(false); setPalette(false); onPriority(value);
    try { const result = await setTaskPriority(target.id, value); onPriority(result.priority); window.dispatchEvent(new CustomEvent("hk-priority-undo", { detail: { id: target.id, previous: result.previous, current: result.priority, onPriority } })); }
    catch { onPriority(before); window.dispatchEvent(new Event("hk-priority-error")); } finally { setPending(false); }
  };
  return <span ref={root} className="task-actions" onKeyDown={e => { if (e.key === "Escape") { setOpen(false); setPalette(false); } }}>
    {actions.filter(a => a.applicable(target)).map(a => <button key={a.id} className={compact ? "action-menu-button" : undefined} aria-label={compact ? `작업 #${target.id} 우선순위 변경` : undefined} aria-expanded={open} disabled={pending || !!a.disabledReason(target)} title={a.disabledReason(target) ?? a.label} onClick={() => setOpen(!open)}>{compact ? <span aria-hidden="true">⋯</span> : `${a.label} (${a.shortcut})`}</button>)}
    {open && <span className="priority-menu" role="menu" aria-label="우선순위 선택" onKeyDown={e => { if (!e.metaKey && !e.ctrlKey && !e.altKey && /^[0-4]$/.test(e.key)) { e.preventDefault(); void change(Number(e.key)); } }}><span className="priority-menu-heading">우선순위 선택</span>{[1,2,3,4,0].map((p, i) => <button key={p} autoFocus={i === 0} role="menuitemradio" aria-checked={target.priority === p} onClick={() => void change(p)}><span>{p === 0 ? "없음" : `P${p}`}{target.priority === p ? " · 현재" : ""}</span><kbd>{p}</kbd></button>)}</span>}
    {palette && <span className="action-palette" role="dialog" aria-label="명령 팔레트"><label>작업 #{target.id} 명령 <input autoFocus value={query} onChange={e => setQuery(e.target.value)} placeholder="명령 검색" /></label>{actions.filter(a => a.applicable(target) && [a.label, ...a.keywords].some(word => word.toLowerCase().includes(query.toLowerCase()))).map(a => <button key={a.id} disabled={!!a.disabledReason(target)} title={a.disabledReason(target) ?? ""} onClick={() => { setPalette(false); setQuery(""); setOpen(true); }}>{a.label} · {a.shortcut}</button>)}</span>}
    <ActionKeys root={root} target={target} open={() => setOpen(true)} palette={() => setPalette(true)} />
  </span>;
}
function ActionKeys({ root, target, open, palette }: { root: RefObject<HTMLSpanElement>; target: ActionTarget; open: () => void; palette: () => void }) {
  useEffect(() => { const key = (e: KeyboardEvent) => { const el = document.activeElement; if (!(el instanceof Element) || !root.current?.closest("[data-action-task]")?.contains(el) || isTypingTarget(e.target)) return; if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); palette(); } else if (!e.metaKey && !e.ctrlKey && !e.altKey && e.key === "p" && !actions[0].disabledReason(target)) { e.preventDefault(); open(); } }; window.addEventListener("keydown", key); return () => window.removeEventListener("keydown", key); }, [root, target, open, palette]); return null;
}
