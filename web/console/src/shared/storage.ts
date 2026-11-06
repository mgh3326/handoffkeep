// The one localStorage access path for the console. Console-wide keys are
// listed here (an app's own key lives next to that app), and each read or
// write resolves localStorage inside
// its try: reading the property itself throws in storage-blocked browsers,
// where the console must still work (preferences just do not persist).

export const STORAGE_KEYS = {
  theme: "hk-console:theme",
  showOriginal: "hk-console:show-original",
  recent: "hk-console:recent",
  workProject: "hk-console:work-project",
  scope: "hk-console:scope",
} as const;

/** The stored string, or null when absent or storage is unavailable. */
export function readStored(key: string, storage?: Pick<Storage, "getItem">): string | null {
  try {
    return (storage ?? globalThis.localStorage)?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

/** Stores value; a failure (blocked or full storage) is ignored. */
export function writeStored(key: string, value: string, storage?: Pick<Storage, "setItem">): void {
  try {
    (storage ?? globalThis.localStorage)?.setItem(key, value);
  } catch {
    /* Preference only: the page works without it. */
  }
}
