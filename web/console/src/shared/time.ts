// Date formatting shared by the console apps. Times are rendered in UTC
// unless a helper says otherwise, so two pages never show the same instant
// differently.

const pad = (n: number) => String(n).padStart(2, "0");

/** "YYYY-MM-DD" in UTC, or "시각 미상" for an unparsable time. */
export function formatDayUTC(iso: string): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return "시각 미상";
  }
  const d = new Date(t);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

/** "YYYY-MM-DD HH:MM UTC", or null for an unparsable time. */
export function formatMinuteUTC(iso: string): string | null {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return null;
  }
  const d = new Date(t);
  return `${formatDayUTC(iso)} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`;
}
