import { readStored, writeStored } from "../shared/storage";
export type Draft = {
  option: string;
  text: string;
  event_key?: string;
  saved_at: string;
};
export const draftKey = (recordId: string, requestId: string) =>
  `hk-console:decision-draft:${recordId}:${requestId}`;
export function readDraft(recordId: string, requestId: string): Draft | null {
  const raw = readStored(draftKey(recordId, requestId));
  if (!raw) return null;
  try {
    const v: unknown = JSON.parse(raw);
    if (
      typeof v === "object" &&
      v !== null &&
      "option" in v &&
      typeof v.option === "string" &&
      "text" in v &&
      typeof v.text === "string" &&
      "saved_at" in v &&
      typeof v.saved_at === "string" &&
      (!("event_key" in v) || typeof v.event_key === "string")
    )
      return v as Draft;
  } catch {
    /* unavailable draft */
  }
  return null;
}
export function saveDraft(recordId: string, requestId: string, draft: Draft) {
  writeStored(draftKey(recordId, requestId), JSON.stringify(draft));
}
