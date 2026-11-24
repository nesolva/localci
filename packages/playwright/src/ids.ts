import { createHash } from "node:crypto";
import * as path from "node:path";

/** Joins describe blocks and the test title the way every event prints them. */
export const TITLE_SEP = " › ";

/**
 * Test id = hash(project, file, title path), stable across line moves.
 * `file` is relative to the Playwright root dir with forward slashes.
 */
export function testId(project: string, file: string, titles: string[]): string {
  return createHash("sha1").update(JSON.stringify([project, file, titles])).digest("hex").slice(0, 16);
}

export function relFile(rootDir: string, file: string): string {
  return path.relative(rootDir, file).split(path.sep).join("/");
}
