// The console theme: dark by default, light when the viewer chose it. Every
// entry calls bootTheme() before its first render so a light-theme page never
// flashes dark.

import { readStored, STORAGE_KEYS, writeStored } from "./storage";

export type Theme = "dark" | "light";

export function loadTheme(storage?: Pick<Storage, "getItem">): Theme {
  return readStored(STORAGE_KEYS.theme, storage) === "light" ? "light" : "dark";
}

export function applyTheme(theme: Theme, storage?: Pick<Storage, "setItem">): void {
  document.documentElement.dataset.theme = theme;
  writeStored(STORAGE_KEYS.theme, theme, storage);
}

/** Applies the stored theme before the first render. */
export function bootTheme(): void {
  if (loadTheme() === "light") {
    document.documentElement.dataset.theme = "light";
  }
}
