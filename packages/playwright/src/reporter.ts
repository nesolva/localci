import type { FullConfig, FullResult, Reporter, Suite, TestCase, TestResult, TestStep } from "@playwright/test/reporter";
import * as fs from "node:fs";
import { COVERAGE_ATTACHMENT } from "./coverage";
import { ARIA_ATTACHMENT, CONTEXT_ATTACHMENT, clip, MAX_LIST, safeUrl, type FailureContext } from "./failure-context";
import { relFile, testId, TITLE_SEP } from "./ids";

type Kind = "step" | "hook" | "expect";
const KINDS: Record<string, Kind | undefined> = { "test.step": "step", hook: "hook", expect: "expect" };

const strip = (s: string) => s.replace(/\u001b\[[0-9;]*m/g, "");

function status(s: TestResult["status"]) {
  return s === "timedOut" ? "timedout" : s;
}

/**
 * What this attempt means for the run. Playwright's `result.status` is what happened; `test.outcome()` says
 * whether that was expected: a `test.fail()` test that fails is a pass, and one that passes is a failure.
 */
export function reportedStatus(
  outcome: ReturnType<TestCase["outcome"]>,
  s: TestResult["status"],
  expected: TestCase["expectedStatus"] = "passed",
) {
  // `flaky` = an earlier attempt was not what the test expected and this one is: a `test.fail()` that passed, then
  // failed on its retry, ends here as the failure it declared.
  if (outcome === "flaky" && s === expected && s !== "passed") return "passed";
  if (outcome === "expected" && (s === "failed" || s === "timedOut")) return "passed";
  if (outcome === "unexpected" && s === "passed") return "failed";
  return status(s);
}

/** Writes protocol v1 events to $LOCALCI_REPORT, one `write` per line. Inert when it is unset. */
export default class LocalciReporter implements Reporter {
  private fd: number | null = null;
  private rootDir = "";
  private stepIds = new WeakMap<TestStep, string>();
  private stepCount = new Map<string, number>();

  constructor() {
    const file = process.env.LOCALCI_REPORT;
    if (file) this.fd = fs.openSync(file, "a");
  }

  printsToStdio() {
    return false;
  }

  private emit(t: string, body: Record<string, unknown>) {
    if (this.fd === null) return;
    // A single write on an O_APPEND descriptor keeps lines whole even with several writers.
    fs.writeSync(this.fd, JSON.stringify({ v: 1, t, ts: Date.now(), ...body }) + "\n");
  }

  private id(test: TestCase): string {
    return testId(test.parent.project()?.name ?? "", relFile(this.rootDir, test.location.file), test.titlePath().slice(3));
  }

  onBegin(config: FullConfig, suite: Suite) {
    this.rootDir = config.rootDir;
    this.emit("begin", { shard: Number(process.env.LOCALCI_SHARD) || 1, total: suite.allTests().length });
  }

  onTestBegin(test: TestCase, result: TestResult) {
    this.emit("test.begin", {
      test: this.id(test),
      file: relFile(this.rootDir, test.location.file),
      title: test.titlePath().slice(3).join(TITLE_SEP),
      line: test.location.line,
      retry: result.retry,
    });
  }

  private emitted(step: TestStep): TestStep | undefined {
    for (let p = step.parent; p; p = p.parent) if (this.stepIds.has(p)) return p;
    return undefined;
  }

  onStepBegin(test: TestCase, _result: TestResult, step: TestStep) {
    const kind = KINDS[step.category];
    if (!kind) return;
    const tid = this.id(test);
    const n = (this.stepCount.get(tid) ?? 0) + 1;
    this.stepCount.set(tid, n);
    const sid = `${tid}:${n}`;
    this.stepIds.set(step, sid);
    const parent = this.emitted(step);
    this.emit("step.begin", { test: tid, step: sid, parent: parent ? this.stepIds.get(parent) : null, title: step.title, kind });
  }

  onStepEnd(_test: TestCase, _result: TestResult, step: TestStep) {
    const sid = this.stepIds.get(step);
    if (!sid) return;
    this.emit("step.end", { step: sid, ms: step.duration, ok: !step.error });
  }

  onTestEnd(test: TestCase, result: TestResult) {
    const tid = this.id(test);
    const attached = (name: string) => result.attachments.find((a) => a.name === name);
    const screenshot = attached("screenshot")?.path ?? null;
    const trace = attached("trace")?.path ?? null;
    const aria = attached(ARIA_ATTACHMENT)?.path ?? null;
    const video = attached("video")?.path ?? null;
    for (const [kind, path] of [["screenshot", screenshot], ["trace", trace], ["video", video], ["aria", aria]] as const) {
      if (path) this.emit("artifact", { test: tid, kind, path });
    }
    const covered = result.attachments.find((a) => a.name === COVERAGE_ATTACHMENT)?.body;
    if (covered) {
      try {
        const body = JSON.parse(covered.toString()) as { files: unknown; incomplete?: unknown };
        if (Array.isArray(body.files))
          this.emit("coverage", {
            test: tid,
            files: body.files.filter((f) => typeof f === "string"),
            ...(body.incomplete === true ? { incomplete: true } : {}),
          });
      } catch {
        // Unreadable coverage is no coverage: the core keeps running the test.
      }
    }
    const final = reportedStatus(test.outcome(), result.status, test.expectedStatus);
    const failed = final !== "passed" && final !== "skipped" && (result.error || final === "failed");
    this.emit("test.end", {
      test: tid,
      status: final,
      ms: result.duration,
      retry: result.retry,
      ...(failed ? { failure: this.failure(test, result, screenshot, trace) } : {}),
    });
  }

  private failure(test: TestCase, result: TestResult, screenshot: string | null, trace: string | null) {
    // A test that times out inside an action reports a bare "Test timeout" first; the error that names the
    // locator, its call log and its location comes after it.
    const error = result.error?.location ? result.error : (result.errors.find((e) => e.location) ?? result.error);
    const message = strip(error?.message ?? "Expected the test to fail, but it passed");
    const body = attachedJSON(result);
    const { expected, actual } = matcherValues(message);
    const loc = error?.location;
    return {
      step: failingStep(result.steps) || test.title,
      message: message.length > 2000 ? message.slice(0, 2000) + "…" : message,
      expected,
      actual,
      location: loc ? `${loc.file}:${loc.line}` : null,
      aria: body?.aria ?? null,
      console: (body?.console ?? []).slice(-MAX_LIST).map((c) => clip(String(c))),
      network: (body?.network ?? []).slice(-MAX_LIST).map((n) => ({ ...n, url: safeUrl(String(n.url)) })),
      screenshot,
      trace,
    };
  }

  onEnd(result: FullResult) {
    this.emit("end", { status: result.status });
    if (this.fd !== null) fs.closeSync(this.fd);
    this.fd = null;
  }
}

function attachedJSON(result: TestResult): FailureContext | null {
  const a = result.attachments.find((x) => x.name === CONTEXT_ATTACHMENT);
  if (!a?.body) return null;
  try {
    return JSON.parse(a.body.toString());
  } catch {
    return null;
  }
}

/** user steps down to the deepest failing one: `Checkout › Expect "toHaveText"`. */
function failingStep(steps: TestStep[]): string {
  const chain: string[] = [];
  let level = steps;
  for (;;) {
    const failed = level.find((s) => s.error && s.category !== "fixture");
    if (!failed) break;
    if (failed.category === "test.step" || failed.steps.every((s) => !s.error)) chain.push(failed.title);
    level = failed.steps;
  }
  return chain.join(TITLE_SEP);
}

/** `Expected: …` / `Received: …` lines of Playwright's matcher message. */
export function matcherValues(message: string): { expected: string | null; actual: string | null } {
  const grab = (label: string) => {
    const m = new RegExp(`^\\s*${label}( string| pattern| value)?:\\s*(.*)$`, "m").exec(message);
    return m ? m[2].trim() : null;
  };
  return { expected: grab("Expected"), actual: grab("Received") };
}
