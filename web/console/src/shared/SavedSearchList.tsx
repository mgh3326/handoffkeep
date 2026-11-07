// Saved searches in the Work tabs area (hk 3364 §4): apply, rename, delete.
// Persistence is the caller's job so the list has one owner (the page).

import { useState } from "react";
import { makeSavedSearch, renameSavedSearch, MAX_SAVED_SEARCHES, type SavedSearch } from "./savedSearches";
import "./savedSearches.css";

export function SavedSearchList({ searches, currentQuery, onApply, onChange }: {
  searches: SavedSearch[];
  currentQuery: string;
  onApply: (query: string) => void;
  onChange: (list: SavedSearch[]) => void;
}) {
  const [saving, setSaving] = useState(false);
  const [name, setName] = useState("");
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const [error, setError] = useState("");

  const startSave = () => {
    setError("");
    if (currentQuery.trim() === "") {
      setError("저장할 검색어가 없습니다.");
      return;
    }
    setName("");
    setSaving(true);
  };

  const save = () => {
    const search = makeSavedSearch(name, currentQuery);
    if (!search) {
      setError("이름과 검색어를 입력하세요.");
      return;
    }
    onChange([...searches, search].slice(-MAX_SAVED_SEARCHES));
    setSaving(false);
    setName("");
    setError("");
  };

  const startRename = (search: SavedSearch) => {
    setRenaming(search.id);
    setRenameName(search.name);
    setError("");
  };

  const rename = (search: SavedSearch) => {
    const next = renameSavedSearch(search, renameName);
    if (!next) {
      setError("이름을 입력하세요.");
      return;
    }
    onChange(searches.map(entry => entry.id === search.id ? next : entry));
    setRenaming(null);
    setError("");
  };

  return (
    <section className="work-saved" aria-label="저장한 검색">
      <div className="work-saved-head">
        <h2>저장한 검색</h2>
        <button type="button" onClick={startSave}>현재 검색 저장</button>
      </div>
      {saving ? (
        <form className="work-saved-form" onSubmit={event => { event.preventDefault(); save(); }}>
          <input autoFocus aria-label="저장 이름" value={name} onChange={event => setName(event.target.value)} placeholder="검색 이름" />
          <button type="submit" disabled={name.trim() === ""}>저장</button>
          <button type="button" className="work-saved-cancel" onClick={() => { setSaving(false); setError(""); }}>취소</button>
        </form>
      ) : null}
      {searches.length === 0 && !saving ? <p className="work-saved-empty">저장한 검색이 없습니다.</p> : null}
      <ul className="work-saved-list">
        {searches.map(search => (
          <li key={search.id}>
            {renaming === search.id ? (
              <form className="work-saved-form" onSubmit={event => { event.preventDefault(); rename(search); }}>
                <input autoFocus aria-label={`${search.name} 새 이름`} value={renameName} onChange={event => setRenameName(event.target.value)} />
                <button type="submit" disabled={renameName.trim() === ""}>저장</button>
                <button type="button" className="work-saved-cancel" onClick={() => { setRenaming(null); setError(""); }}>취소</button>
              </form>
            ) : (
              <>
                <button type="button" className="work-saved-apply" title={search.query} onClick={() => onApply(search.query)}>{search.name}</button>
                <button type="button" aria-label={`${search.name} 이름 변경`} onClick={() => startRename(search)}>이름</button>
                <button type="button" aria-label={`${search.name} 삭제`} onClick={() => onChange(searches.filter(entry => entry.id !== search.id))}>삭제</button>
              </>
            )}
          </li>
        ))}
      </ul>
      {error ? <p role="alert" className="work-saved-error">{error}</p> : null}
    </section>
  );
}
