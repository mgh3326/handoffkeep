import { useState, useSyncExternalStore } from "react";
import { readStored, STORAGE_KEYS, writeStored } from "./storage";

const eventName = "hk-console-original-preference";
function subscribe(notify: () => void) {
  window.addEventListener(eventName, notify);
  window.addEventListener("storage", notify);
  return () => { window.removeEventListener(eventName, notify); window.removeEventListener("storage", notify); };
}
function snapshot() { return readStored(STORAGE_KEYS.showOriginal) === "true"; }
export function useShowOriginal() { return useSyncExternalStore(subscribe, snapshot, () => false); }
export function OriginalPreference() {
  const original = useShowOriginal();
  return <button type="button" aria-pressed={original} onClick={() => { writeStored(STORAGE_KEYS.showOriginal, String(!original)); window.dispatchEvent(new Event(eventName)); }}>원문 전체 보기</button>;
}
export function TranslatedText({ original, korean, stale }: { original: string; korean?: string; stale?: boolean }) {
  const globalOriginal = useShowOriginal();
  const [override, setOverride] = useState<boolean | null>(null);
  const showOriginal = override ?? globalOriginal;
  return <span>{!showOriginal && korean && !stale ? korean : original}{stale && <small> 갱신 필요</small>}{korean && !stale && <button type="button" aria-pressed={showOriginal} onClick={() => setOverride(!showOriginal)}>{showOriginal ? "번역" : "원문"}</button>}</span>;
}
