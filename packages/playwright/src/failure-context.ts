/** What the auto fixture attaches to a failed test and the reporter folds into `failure`. */
export const CONTEXT_ATTACHMENT = "localci-context";
export const ARIA_ATTACHMENT = "localci-aria";

export interface NetworkError {
  method: string;
  url: string;
  status: number;
}

export interface FailureContext {
  console: string[];
  network: NetworkError[];
  aria: string | null;
}

export const MAX_ARIA_LINES = 40;
export const MAX_LIST = 5;
/** Longest console entry, request URL or other free-text evidence field that leaves the test process. */
export const MAX_FIELD = 500;

/** `s` cut to `max` characters, with an ellipsis when it was longer. */
export function clip(s: string, max = MAX_FIELD): string {
  return s.length > max ? s.slice(0, max) + "…" : s;
}

/**
 * A URL as evidence: origin and path only. Query strings and fragments carry tokens, signed URLs and
 * OAuth codes, and userinfo carries passwords; none of them belongs on a dashboard.
 */
export function safeUrl(raw: string): string {
  try {
    const u = new URL(raw);
    if (u.protocol === "data:" || u.protocol === "blob:" || u.protocol === "javascript:") return u.protocol + "…";
    return clip(u.origin === "null" ? u.protocol + u.pathname : u.origin + u.pathname);
  } catch {
    return clip(raw.split(/[?#]/)[0]);
  }
}

/** At most MAX_ARIA_LINES lines, centred on the first line that mentions `needle`. */
export function excerpt(snapshot: string, needle: string | null): string {
  const lines = snapshot.split("\n");
  if (lines.length <= MAX_ARIA_LINES) return snapshot;
  const at = needle ? lines.findIndex((l) => l.includes(needle)) : -1;
  if (at < 0) return lines.slice(0, MAX_ARIA_LINES).join("\n");
  const start = Math.max(0, Math.min(at - MAX_ARIA_LINES / 2, lines.length - MAX_ARIA_LINES));
  return lines.slice(start, start + MAX_ARIA_LINES).join("\n");
}
