// The query text input shared by Work and Home. It owns only the caret and
// the suggestion list; the page owns the filters, so a control change and a
// typed query stay in sync through the same state.

import { useId, useState, type KeyboardEvent, type ReactNode } from "react";
import { Icon } from "../Icon";
import { querySuggestions, type QueryContext, type QuerySuggestion } from "./query";
import "./query.css";

export type QueryBarProps = {
  value: string;
  onChange: (value: string) => void;
  onCommit?: (value: string) => void;
  errors: string[];
  context: QueryContext;
  label?: string;
  placeholder?: string;
  /** Rendered at the right end of the bar (Work puts 「현재 검색 저장」 here). */
  actions?: ReactNode;
};

export function QueryBar({ value, onChange, onCommit, errors, context, label = "검색 쿼리", placeholder = '"검색어" 상태:개발 중 레인:console-demo #진행중', actions }: QueryBarProps) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  // While typing only the autocomplete speaks; a half-typed "상태:" is not an
  // error yet. The refusal shows once the text is applied (Enter) or left
  // (blur), and hides again at the next keystroke.
  const [judged, setJudged] = useState(false);
  const listID = useId();
  const suggestions = querySuggestions(value, context);
  const shown = open ? suggestions.items : [];

  const complete = (item: QuerySuggestion) => {
    onChange(`${value.slice(0, suggestions.start)}${item.insert}${value.slice(suggestions.end)}`);
    setActive(0);
    setJudged(false);
  };

  const onKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing) return;
    if (event.key === "Escape") {
      if (shown.length > 0) {
        event.preventDefault();
        setOpen(false);
      }
      return;
    }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      if (shown.length === 0) return;
      event.preventDefault();
      setActive(current => {
        const delta = event.key === "ArrowDown" ? 1 : -1;
        return (current + delta + shown.length) % shown.length;
      });
      return;
    }
    if (event.key === "Tab" && shown[active]) {
      event.preventDefault();
      complete(shown[active]);
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      setOpen(false);
      setJudged(true);
      onCommit?.(value);
    }
  };

  return (
    <div className="hk-query">
      <div className="hk-query-row">
        <label className="hk-query-field">
          <Icon name="magnifying-glass" />
          <span className="hk-sr-only">{label}</span>
          <input
            value={value}
            onChange={event => {
              onChange(event.target.value);
              setActive(0);
              setOpen(true);
              setJudged(false);
            }}
            onKeyDown={onKey}
            onFocus={() => setOpen(true)}
            onBlur={() => { setOpen(false); setJudged(true); }}
            aria-label={label}
            aria-autocomplete="list"
            aria-expanded={shown.length > 0}
            aria-controls={shown.length > 0 ? listID : undefined}
            placeholder={placeholder}
            autoComplete="off"
            spellCheck={false}
          />
        </label>
        {actions}
      </div>
      {shown.length > 0 ? (
        <ul id={listID} role="listbox" aria-label="쿼리 자동완성" className="hk-query-suggestions">
          {shown.map((item, index) => (
            <li key={`${item.value}:${index}`} role="presentation">
              <button
                type="button"
                role="option"
                aria-selected={index === active}
                className={index === active ? "is-active" : undefined}
                onMouseDown={event => event.preventDefault()}
                onClick={() => complete(item)}
              >
                {item.label}
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      {judged && errors.length > 0 ? (
        <div role="alert" className="hk-query-error">
          {errors.map((error, index) => <p key={index}>{error}</p>)}
        </div>
      ) : null}
    </div>
  );
}
