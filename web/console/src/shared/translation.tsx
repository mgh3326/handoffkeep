import { useEffect, useState, useSyncExternalStore } from "react";
import { readStored, STORAGE_KEYS, writeStored } from "./storage";

const eventName = "hk-console-original-preference";
function subscribe(notify: () => void) {
  window.addEventListener(eventName, notify);
  window.addEventListener("storage", notify);
  return () => { window.removeEventListener(eventName, notify); window.removeEventListener("storage", notify); };
}
function snapshot() { return readStored(STORAGE_KEYS.showOriginal) === "true"; }
export function useShowOriginal() { return useSyncExternalStore(subscribe, snapshot, () => false); }
// This action can also be presented by a row action menu.
export const translationAction = {
  id: "show-original",
  label: "번역",
  keywords: ["번역", "원문"],
  shortcut: null,
  disabledReason: (korean?: string, stale?: boolean) => !korean ? "번역 없음" : stale ? "갱신 필요" : null,
  applicable: (korean?: string, stale?: boolean) => Boolean(korean && !stale),
  execute: (expanded: boolean) => !expanded,
};
export function TranslationIndicator({ expanded, onChange }: { expanded: boolean; onChange: (expanded: boolean) => void }) {
  return <button className="hk-translation-indicator" type="button" aria-label="원문 보기" aria-expanded={expanded} onClick={() => onChange(translationAction.execute(expanded))}>{translationAction.label}</button>;
}
export function OriginalPreference() {
  const original = useShowOriginal();
  return <button className="hk-shell-original" type="button" aria-pressed={original} onClick={() => { writeStored(STORAGE_KEYS.showOriginal, String(!original)); window.dispatchEvent(new Event(eventName)); }}>원문 보기</button>;
}
export function TranslatedText({ original, korean, stale }: { original: string; korean?: string; stale?: boolean }) {
  const globalOriginal = useShowOriginal();
  const [override, setOverride] = useState<boolean | null>(null);
  useEffect(() => setOverride(null), [globalOriginal]);
  const showOriginal = override ?? globalOriginal;
  const translated = translationAction.applicable(korean, stale);
  return <span>{translated ? korean : original}{stale && <small className="hk-translation-stale"> 갱신 필요</small>}{translated && <><TranslationIndicator expanded={showOriginal} onChange={setOverride} />{showOriginal && <span className="hk-original-text">{original}</span>}</>}</span>;
}
