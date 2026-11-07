// Saved Work/Home queries (hk 3364 §4). They live in this browser for now;
// server-side saved searches are deliberately out of scope. A malformed
// stored entry is dropped rather than rendered as a broken search.

import { readStored, STORAGE_KEYS, writeStored } from "./storage";

export type SavedSearch = { id: string; name: string; query: string };

export const MAX_SAVED_SEARCHES = 50;
const MAX_NAME = 60;
const MAX_QUERY = 500;

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function trim(value: string, limit: number): string {
  return value.trim().slice(0, limit);
}

function newID(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
}

export function makeSavedSearch(name: string, query: string): SavedSearch | null {
  const trimmedName = trim(name, MAX_NAME);
  const trimmedQuery = trim(query, MAX_QUERY);
  if (trimmedName === "" || trimmedQuery === "") return null;
  return { id: newID(), name: trimmedName, query: trimmedQuery };
}

export function renameSavedSearch(search: SavedSearch, name: string): SavedSearch | null {
  const trimmed = trim(name, MAX_NAME);
  return trimmed === "" ? null : { ...search, name: trimmed };
}

export function readSavedSearches(storage?: Pick<Storage, "getItem">): SavedSearch[] {
  const raw = readStored(STORAGE_KEYS.savedSearches, storage);
  if (raw === null) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const seen = new Set<string>();
  const out: SavedSearch[] = [];
  for (const entry of parsed) {
    if (!record(entry)) continue;
    if (typeof entry.id !== "string" || typeof entry.name !== "string" || typeof entry.query !== "string") continue;
    const id = entry.id.trim();
    const name = trim(entry.name, MAX_NAME);
    const query = trim(entry.query, MAX_QUERY);
    if (id === "" || name === "" || query === "" || seen.has(id)) continue;
    seen.add(id);
    out.push({ id, name, query });
    if (out.length >= MAX_SAVED_SEARCHES) break;
  }
  return out;
}

export function writeSavedSearches(list: SavedSearch[], storage?: Pick<Storage, "setItem">): void {
  writeStored(STORAGE_KEYS.savedSearches, JSON.stringify(list.slice(0, MAX_SAVED_SEARCHES)), storage);
}
