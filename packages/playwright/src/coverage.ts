import type { BrowserContext, Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";

/** The attachment the fixture leaves on a test for the reporter: `{ files }`. */
export const COVERAGE_ATTACHMENT = "localci-coverage";

const MAX_FILES = 2000;

type Segment = [genCol: number, source: number, line: number, col: number];
interface SourceMap {
  /** A repo path; "" for a dependency or bundler runtime (not the repo's, left out on purpose); null when it cannot be told. */
  sources: (string | null)[];
  lines: Segment[][]; // by generated line, sorted by column
}
interface V8Entry {
  url: string;
  source?: string;
  functions: { ranges: { startOffset: number; endOffset: number; count: number }[] }[];
}

const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

/** Decodes the `mappings` of a source map (VLQ), keeping the segments that name a source. */
export function decodeMappings(mappings: string): Segment[][] {
  const out: Segment[][] = [];
  let source = 0, line = 0, col = 0;
  for (const part of mappings.split(";")) {
    let genCol = 0;
    const segs: Segment[] = [];
    for (const seg of part.split(",")) {
      if (!seg) continue;
      const nums: number[] = [];
      let value = 0, shift = 0;
      for (const ch of seg) {
        const d = B64.indexOf(ch);
        if (d < 0) break;
        value += (d & 31) << shift;
        if (d & 32) {
          shift += 5;
        } else {
          nums.push(value & 1 ? -(value >> 1) : value >> 1);
          value = 0;
          shift = 0;
        }
      }
      if (nums.length === 0) continue;
      genCol += nums[0];
      if (nums.length >= 4) {
        source += nums[1];
        line += nums[2];
        col += nums[3];
        segs.push([genCol, source, line, col]);
      }
    }
    out.push(segs);
  }
  return out;
}

/** The source a generated position came from, or -1. */
export function lookup(map: SourceMap, line: number, col: number): number {
  const segs = map.lines[line];
  if (!segs?.length) return -1;
  let lo = 0, hi = segs.length - 1, best = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (segs[mid][0] <= col) {
      best = mid;
      lo = mid + 1;
    } else hi = mid - 1;
  }
  return best < 0 ? -1 : segs[best][1];
}

type Placed = { path: string } | "dependency" | "unknown";

/** Where a source name points: a repo path, a dependency or bundler runtime (left out on purpose), or nowhere known. */
function place(name: string, root = ""): Placed {
  let p: string;
  if (name.startsWith("file://")) p = decodeURIComponent(name.slice(7));
  else {
    p = name.replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]*\//i, "");
    if (p === name && root && !path.posix.isAbsolute(p)) p = path.posix.join(root, p);
  }
  p = p.split("?")[0].replace(/^\.\//, "");
  if (!p) return "unknown";
  if (p.includes("node_modules/") || p.startsWith("webpack/") || p.includes("(webpack)") || p.startsWith("<")) return "dependency";
  return { path: p };
}

/** A source name from a map as a path: bundler prefixes and queries go, the rest is left to the core to place. */
export function sourcePath(name: string, root = ""): string | null {
  const p = place(name, root);
  return typeof p === "string" ? null : p.path;
}

const maps = new Map<string, SourceMap | null>();

async function mapFor(context: BrowserContext, entry: V8Entry): Promise<SourceMap | null> {
  const key = entry.url;
  if (maps.has(key)) return maps.get(key) ?? null;
  let raw: string | null = null;
  const ref = /\/\/[#@] sourceMappingURL=(\S+)\s*$/.exec(entry.source ?? "")?.[1];
  try {
    if (ref?.startsWith("data:")) {
      const comma = ref.indexOf(",");
      const body = ref.slice(comma + 1);
      raw = /;base64/.test(ref.slice(0, comma)) ? Buffer.from(body, "base64").toString("utf8") : decodeURIComponent(body);
    } else if (ref && /^https?:/.test(entry.url)) {
      const res = await context.request.get(new URL(ref, entry.url).href, { timeout: 5000 });
      if (res.ok()) raw = await res.text();
    }
  } catch {
    raw = null;
  }
  let map: SourceMap | null = null;
  if (raw) {
    try {
      const j = JSON.parse(raw) as { sources?: string[]; sourceRoot?: string; mappings?: string };
      if (j.mappings && j.sources) {
        const root = j.sourceRoot ?? "";
        const sources = j.sources.map((s) => {
          const p = place(s, root);
          return p === "dependency" ? "" : p === "unknown" ? null : p.path;
        });
        map = { sources, lines: decodeMappings(j.mappings) };
      }
    } catch {
      map = null;
    }
  }
  maps.set(key, map);
  return map;
}

/** The files a test ran, and whether some source it ran could not be attributed (so the set has a hole). */
export interface Covered {
  files: string[];
  incomplete: boolean;
}

/**
 * Which script files ran: through the source map where there is one, else the script's own path. Dependencies and
 * bundler runtime are left out on purpose; code that ran and cannot be attributed makes the result `incomplete`, so
 * the core never treats the set as the whole of what the test reaches.
 */
export async function filesOf(context: BrowserContext, entries: V8Entry[]): Promise<Covered> {
  const files = new Set<string>();
  let incomplete = false;
  for (const e of entries) {
    if (!/^https?:/.test(e.url)) continue;
    const ran = e.functions.filter((f) => (f.ranges[0]?.count ?? 0) > 0);
    if (ran.length === 0) continue;
    const map = await mapFor(context, e);
    if (!map || !e.source) {
      const p = place(new URL(e.url).pathname.replace(/^\//, ""));
      if (typeof p !== "string") files.add(p.path);
      else if (p === "unknown") incomplete = true;
      continue;
    }
    // Offsets to line/column, once.
    const starts = [0];
    for (let i = 0; i < e.source.length; i++) if (e.source.charCodeAt(i) === 10) starts.push(i + 1);
    for (const f of ran) {
      const off = f.ranges[0].startOffset;
      let lo = 0, hi = starts.length - 1;
      while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if (starts[mid] <= off) lo = mid;
        else hi = mid - 1;
      }
      const s = lookup(map, lo, off - starts[lo]);
      const src = s >= 0 ? map.sources[s] : null;
      if (src) files.add(src);
      else if (src === null) incomplete = true;
    }
  }
  const all = [...files].sort();
  return { files: all.slice(0, MAX_FILES), incomplete: incomplete || all.length > MAX_FILES };
}

/**
 * Starts JS coverage in every page of the context (Chromium only) and returns what ends it: the files the test
 * ran (and whether they are all of them), or null when coverage is not available. Dumps go under $LOCALCI_OUTPUT_DIR, never the workspace.
 */
export async function trackCoverage(context: BrowserContext, test: string): Promise<() => Promise<Covered | null>> {
  const started = new Set<Page>();
  const start = async (page: Page) => {
    if (started.has(page) || !page.coverage) return;
    started.add(page);
    try {
      await page.coverage.startJSCoverage({ resetOnNavigation: false });
    } catch {
      started.delete(page);
    }
  };
  context.on("page", (p) => void start(p));
  await Promise.all(context.pages().map(start));
  return async () => {
    if (started.size === 0) return null;
    const entries: V8Entry[] = [];
    let lost = false;
    for (const page of started) {
      if (page.isClosed()) {
        lost = true; // a page that went away mid-test took what it ran with it
        continue;
      }
      try {
        entries.push(...((await page.coverage.stopJSCoverage()) as V8Entry[]));
      } catch {
        lost = true;
      }
    }
    const covered = await filesOf(context, entries);
    covered.incomplete ||= lost;
    const { files } = covered;
    const out = process.env.LOCALCI_OUTPUT_DIR;
    if (out) {
      try {
        fs.mkdirSync(path.join(out, "coverage"), { recursive: true });
        fs.writeFileSync(
          path.join(out, "coverage", `${test.replace(/[^A-Za-z0-9._-]/g, "_")}.web.json`),
          JSON.stringify({ test, scripts: entries.map((e) => e.url), files, incomplete: covered.incomplete }),
        );
      } catch {
        // Dumps are for people looking at a run; they never fail a test.
      }
    }
    return covered;
  };
}
