// The command bar (hk 3364 §2): one line, a preview for every target, then
// Enter applies through the existing routes. Nothing is sent before the
// preview exists; refusals are shown per task and are never attempted.

import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { fetchLanes } from "../boardApi";
import { dialogKeys } from "../actions/dialogs";
import type { ActionTarget } from "../actions";
import { applyCommand, type ApplyResult } from "./apply";
import { commandSuggestions, parseCommand, type CommandSuggestion } from "./parse";
import { previewCommand, type PreviewRow } from "./preview";
import "./command.css";

export type CommandBarProps = {
  targets: ActionTarget[];
  /** Targets dropped by the caller's cap, shown so the operator knows. */
  truncated?: number;
  initialText?: string;
  /** Tests pass the board's lanes; the page lets the bar fetch them. */
  lanes?: string[];
  onClose: () => void;
  onApplied: () => void;
};

function targetLabel(target: ActionTarget): string {
  return target.title ? `#${target.id} ${target.title}` : `#${target.id}`;
}

function groupPreview(targets: ActionTarget[], rows: PreviewRow[]): { target: ActionTarget; rows: PreviewRow[] }[] {
  return targets.map(target => ({ target, rows: rows.filter(row => row.target === target) }));
}

function failedLine(row: { target: ActionTarget; change: PreviewRow["change"]; message: string }, preview: PreviewRow[]): string {
  const match = preview.find(cell => cell.target === row.target && cell.change === row.change);
  return `${targetLabel(row.target)} · ${match?.field ?? "변경"} — ${row.message}`;
}

export function CommandBar({ targets, truncated = 0, initialText = "", lanes: lanesProp, onClose, onApplied }: CommandBarProps) {
  const [text, setText] = useState(initialText);
  const [lanes, setLanes] = useState<string[]>(lanesProp ?? []);
  const [lanesFailed, setLanesFailed] = useState(false);
  const [active, setActive] = useState(0);
  const [suggestionsOpen, setSuggestionsOpen] = useState(false);
  const [phase, setPhase] = useState<"edit" | "busy" | "done">("edit");
  const [result, setResult] = useState<ApplyResult | null>(null);
  const [undoMessage, setUndoMessage] = useState("");
  const [runError, setRunError] = useState("");
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    input.current?.focus();
  }, []);

  useEffect(() => {
    if (lanesProp !== undefined) {
      setLanes(lanesProp);
      return;
    }
    let activeRequest = true;
    void fetchLanes()
      .then(list => {
        if (activeRequest) setLanes(list);
      })
      .catch(() => {
        if (activeRequest) setLanesFailed(true);
      });
    return () => {
      activeRequest = false;
    };
  }, [lanesProp]);

  const parsed = useMemo(() => parseCommand(text, { lanes }), [text, lanes]);
  const preview = useMemo(() => previewCommand(targets, parsed.changes, { lanes }), [targets, parsed.changes, lanes]);
  const suggestions = useMemo(() => commandSuggestions(text, { lanes }), [text, lanes]);
  const canApply = phase === "edit" && parsed.errors.length === 0 && preview.runnable > 0;

  // One Escape handler for the whole bar, wherever focus sits. The first
  // Escape closes the suggestion list, the second closes the bar.
  useEffect(() => {
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      if (suggestionsOpen && suggestions.items.length > 0) {
        setSuggestionsOpen(false);
        return;
      }
      onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [suggestionsOpen, suggestions.items.length, onClose]);

  const complete = (item: CommandSuggestion) => {
    setText(current => `${current.slice(0, suggestions.start)}${item.insert}${current.slice(suggestions.end)}`);
    setActive(0);
    setSuggestionsOpen(true);
    input.current?.focus();
  };

  const onInputKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      if (suggestions.items.length === 0) return;
      event.preventDefault();
      event.stopPropagation();
      setSuggestionsOpen(true);
      setActive(current => {
        const delta = event.key === "ArrowDown" ? 1 : -1;
        return (current + delta + suggestions.items.length) % suggestions.items.length;
      });
      return;
    }
    if (event.key === "Tab" && suggestionsOpen && suggestions.items[active]) {
      event.preventDefault();
      event.stopPropagation();
      complete(suggestions.items[active]);
      return;
    }
    if (event.key === "Enter" && canApply) {
      event.preventDefault();
      event.stopPropagation();
      void run();
      return;
    }
    if (event.key === "Enter" && suggestionsOpen && suggestions.items[active]) {
      event.preventDefault();
      event.stopPropagation();
      complete(suggestions.items[active]);
    }
  };

  const run = async () => {
    if (!canApply) return;
    setPhase("busy");
    setRunError("");
    try {
      const applied = await applyCommand(targets, parsed.changes, { lanes });
      setResult(applied);
      setPhase("done");
      onApplied();
    } catch {
      setRunError("명령을 적용하지 못했습니다. 잠시 후 다시 시도하세요.");
      setPhase("edit");
    }
  };

  const undo = async () => {
    if (!result?.undo) return;
    setUndoMessage("실행 취소 중…");
    try {
      const undone = await result.undo();
      setUndoMessage(undone.summary);
      setResult(current => current ? { ...current, undo: null } : current);
      onApplied();
    } catch {
      setUndoMessage("실행 취소하지 못했습니다.");
    }
  };

  const grouped = groupPreview(targets, preview.rows);
  const shownSuggestions = suggestionsOpen ? suggestions.items : [];

  /** Tab stays inside the dialog; Escape is handled once by the document
   * listener above. */
  const onDialogKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Tab") dialogKeys(event, onClose);
  };

  return (
    <div className="inbox-backdrop command-backdrop" onClick={phase === "busy" ? undefined : onClose}>
      <div
        role="dialog"
        aria-label="명령 바"
        className="inbox-dialog command-bar"
        onClick={event => event.stopPropagation()}
        onKeyDown={onDialogKey}
      >
        <header className="command-head">
          <h2>명령</h2>
          <p>
            대상 {targets.length}건{truncated > 0 ? ` · 선택 ${targets.length + truncated}건 중 앞 ${targets.length}건` : ""}
          </p>
        </header>

        {phase !== "done" ? (
          <>
            <label className="command-input">
              <span>한 줄 명령</span>
              <input
                ref={input}
                value={text}
                readOnly={phase === "busy"}
                onChange={event => {
                  setText(event.target.value);
                  setActive(0);
                  setSuggestionsOpen(true);
                }}
                onKeyDown={onInputKey}
                onFocus={() => setSuggestionsOpen(true)}
                aria-label="명령 입력"
                aria-autocomplete="list"
                aria-expanded={shownSuggestions.length > 0}
                aria-controls="command-suggestions"
                placeholder="예: 상태 검토 중 우선순위 1 / 개발중 레인 console-demo / 보류"
              />
            </label>
            {shownSuggestions.length > 0 ? (
              <ul id="command-suggestions" role="listbox" aria-label="명령 자동완성" className="command-suggestions">
                {shownSuggestions.map((item, index) => (
                  <li key={item.value} role="presentation">
                    <button type="button" role="option" aria-selected={index === active} onMouseDown={event => event.preventDefault()} onClick={() => complete(item)} className={index === active ? "is-active" : undefined}>
                      {item.label}
                    </button>
                  </li>
                ))}
              </ul>
            ) : null}
            {lanesFailed ? <p className="command-note" role="alert">레인 목록을 불러오지 못해 레인 값은 확인하지 못합니다.</p> : null}
          </>
        ) : null}

        {runError ? <p role="alert" className="command-note">{runError}</p> : null}

        {phase === "done" && result ? (
          <div className="command-result" role="status">
            <p className="command-result-summary">{result.summary}</p>
            {undoMessage ? <p className="command-note">{undoMessage}</p> : null}
            {(result.refused.length > 0 || result.failed.length > 0) ? (
              <ul className="command-result-list">
                {result.refused.map((row, index) => (
                  <li key={`refused:${index}`} className="command-refused">
                    {targetLabel(row.target)} · {row.field} — {row.refusal}
                  </li>
                ))}
                {result.failed.map((row, index) => (
                  <li key={`failed:${index}`} className="command-refused">
                    {failedLine(row, preview.rows)}
                  </li>
                ))}
              </ul>
            ) : null}
            <div className="command-actions">
              {result.undo ? <button type="button" onClick={() => void undo()}>{undoMessage === "" ? "실행 취소" : "다시 실행 취소"}</button> : null}
              <button type="button" className="command-secondary" autoFocus onClick={onClose}>닫기</button>
            </div>
          </div>
        ) : (
          <>
            {parsed.errors.length > 0 ? (
              <div role="alert" className="command-errors">
                {parsed.errors.map((error, index) => (
                  <p key={index}>{error.token ? `"${error.token}" — ` : ""}{error.message}</p>
                ))}
              </div>
            ) : parsed.changes.length === 0 ? (
              <p className="command-note">
                항목 이름을 먼저 쓰거나 값을 그대로 써도 됩니다: 상태·우선순위·레인·보류·보류해제·머지됨·드롭·코멘트.
              </p>
            ) : (
              <div className="command-preview">
                {grouped.map(group => (
                  <section key={group.target.id} aria-label={targetLabel(group.target)}>
                    <h3>{targetLabel(group.target)}</h3>
                    <ul>
                      {group.rows.map((row, index) => (
                        <li key={index} className={row.refusal ? "command-row command-refused" : "command-row"}>
                          <span className="command-field">{row.field}</span>
                          <span className="command-values">{row.from} → {row.to}</span>
                          {row.refusal ? <span className="command-refusal">{row.refusal}</span> : null}
                        </li>
                      ))}
                    </ul>
                  </section>
                ))}
              </div>
            )}
          </>
        )}

        {phase !== "done" ? (
          <div className="command-actions">
            <span className="command-count">
              실행 {preview.runnable}건{preview.refused > 0 ? ` · 거부 ${preview.refused}건` : ""}
            </span>
            <button type="button" className="command-secondary" onClick={onClose} disabled={phase === "busy"}>취소</button>
            <button type="button" onClick={() => void run()} disabled={!canApply}>{phase === "busy" ? "적용 중…" : "적용 (Enter)"}</button>
          </div>
        ) : null}
      </div>
    </div>
  );
}
