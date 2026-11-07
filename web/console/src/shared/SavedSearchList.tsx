// Saved searches (hk 3364 §4). The save control sits at the right end of the
// query bar; the saved searches are one compact chip row under the Work tabs.
// Persistence is the caller's job so the list has one owner (the page).

import { useState } from "react";
import { makeSavedSearch, renameSavedSearch, MAX_QUERY, MAX_SAVED_SEARCHES, type SavedSearch } from "./savedSearches";
import "./savedSearches.css";

/** 「현재 검색 저장」 and its name form, for the query bar's action slot. */
export function SaveSearch({ searches, currentQuery, onChange }: {
  searches: SavedSearch[];
  currentQuery: string;
  onChange: (list: SavedSearch[]) => void;
}) {
  const [saving, setSaving] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState("");

  const startSave = () => {
    setError("");
    if (currentQuery.trim() === "") {
      setError("저장할 검색어가 없습니다.");
      return;
    }
    if (currentQuery.trim().length > MAX_QUERY) {
      setError(`검색어가 너무 깁니다 (${MAX_QUERY}자 이하).`);
      return;
    }
    if (searches.length >= MAX_SAVED_SEARCHES) {
      setError(`저장한 검색은 ${MAX_SAVED_SEARCHES}개까지입니다. 하나를 삭제한 뒤 저장하세요.`);
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
    onChange([...searches, search]);
    setSaving(false);
    setName("");
    setError("");
  };

  return (
    <div className="work-save">
      {saving ? (
        <form className="work-save-form" onSubmit={event => { event.preventDefault(); save(); }}>
          <input autoFocus aria-label="저장 이름" value={name} onChange={event => setName(event.target.value)} placeholder="검색 이름" />
          <button type="submit" disabled={name.trim() === ""}>저장</button>
          <button type="button" className="work-saved-cancel" onClick={() => { setSaving(false); setError(""); }}>취소</button>
        </form>
      ) : (
        <button type="button" onClick={startSave}>현재 검색 저장</button>
      )}
      {error ? <p role="alert" className="work-saved-error">{error}</p> : null}
    </div>
  );
}

/** The saved searches as one chip row: apply, rename, delete. Renders nothing
 * when there are none. */
export function SavedSearchChips({ searches, onApply, onChange }: {
  searches: SavedSearch[];
  onApply: (query: string) => void;
  onChange: (list: SavedSearch[]) => void;
}) {
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const [error, setError] = useState("");

  if (searches.length === 0) return null;

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
    <div className="work-saved" role="group" aria-label="저장한 검색">
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
                <button type="button" aria-label={`${search.name} 이름 바꾸기`} onClick={() => startRename(search)}>이름 바꾸기</button>
                <button type="button" aria-label={`${search.name} 삭제`} onClick={() => onChange(searches.filter(entry => entry.id !== search.id))}>삭제</button>
              </>
            )}
          </li>
        ))}
      </ul>
      {error ? <p role="alert" className="work-saved-error">{error}</p> : null}
    </div>
  );
}
