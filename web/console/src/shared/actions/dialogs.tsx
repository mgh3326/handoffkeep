import { useEffect, type KeyboardEvent, type ReactNode } from "react";
import { isTypingTarget } from "../search";

/** Focus stays inside an open dialog; Escape closes it. */
export function dialogKeys(e: KeyboardEvent<HTMLDialogElement>, close: () => void) {
  if (e.key === "Escape") {
    e.preventDefault();
    close();
    return;
  }
  if (e.key !== "Tab") return;
  const buttons = e.currentTarget.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled)');
  if (!buttons.length) return;
  const first = buttons[0];
  const last = buttons[buttons.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

export function UndoToast({ undo, close, message = "우선순위를 변경했습니다" }: { undo: () => void; close: () => void; message?: string }) {
  useEffect(() => { window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: `${message}. 실행 취소하려면 z를 누르세요.` })); }, [message]);
  useEffect(() => {
    const timer = window.setTimeout(close, 10000);
    const key = (e: globalThis.KeyboardEvent) => {
      if (e.key === "z" && !e.metaKey && !e.ctrlKey && !isTypingTarget(e.target)) {
        e.preventDefault();
        undo();
      }
    };
    window.addEventListener("keydown", key);
    return () => { window.clearTimeout(timer); window.removeEventListener("keydown", key); };
  }, [undo, close]);
  return <div className="action-toast">{message} <button onClick={undo}>실행 취소 (z)</button></div>;
}

export function ConfirmDialog({ title, effect, close, confirm }: { title: string; effect: string; close: () => void; confirm: () => void }) {
  return <dialog open aria-label={title} onKeyDown={e => dialogKeys(e, close)}><h2>{title}</h2><button autoFocus onClick={close}>취소</button><button onClick={confirm}>{effect}</button></dialog>;
}

/** The lifecycle dialogs' shell: a titled form whose confirm button is the
 * effect's name and whose cancel is always the safe first focus. */
export function ActionDialog({ title, effect, close, confirm, pending, confirmDisabled, children }: {
  title: string; effect: string; close: () => void; confirm: () => void;
  pending: boolean; confirmDisabled?: boolean; children: ReactNode;
}) {
  return <dialog open aria-label={title} className="action-dialog" onKeyDown={e => dialogKeys(e, close)}>
    <h2>{title}</h2>
    {children}
    <div className="action-dialog-buttons">
      <button autoFocus onClick={close}>취소</button>
      <button onClick={confirm} disabled={pending || confirmDisabled}>{effect}</button>
    </div>
  </dialog>;
}
