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

/** "YYYY-MM-DD HH:MM KST", or null for an unparsable time. */
export function formatMinuteKST(iso: string): string | null {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return null;
  const d = new Date(t + 9 * 60 * 60 * 1000);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} KST`;
}

/** Compact Korean age ("45초", "12분", "3시간 5분", "2일"); negatives clamp to 0. */
export function formatAge(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s}초`;
  const minutes = Math.floor(s / 60);
  if (minutes < 60) return `${minutes}분`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const rem = minutes % 60;
    return rem > 0 ? `${hours}시간 ${rem}분` : `${hours}시간`;
  }
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return remHours > 0 ? `${days}일 ${remHours}시간` : `${days}일`;
}
