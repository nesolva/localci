import { test as base, expect, type BrowserContext, type Page, type TestInfo } from "@playwright/test";
import * as fs from "node:fs";
import { flowFixture, type FlowCaller } from "./flows";
import { ARIA_ATTACHMENT, CONTEXT_ATTACHMENT, clip, excerpt, MAX_LIST, safeUrl, type FailureContext, type NetworkError } from "./failure-context";
import { tenantFixture, type Tenant } from "./tenant";
import { COVERAGE_ATTACHMENT, trackCoverage } from "./coverage";
import { relFile, testId } from "./ids";
import { locatorFromText, locatorTextFromMessage } from "./locator-text";

const LANDMARKS = [
  "self::main",
  "self::nav",
  "self::header",
  "self::footer",
  "self::aside",
  "self::form",
  "self::dialog",
  "self::section[@aria-label or @aria-labelledby]",
  "@role='main'",
  "@role='navigation'",
  "@role='banner'",
  "@role='contentinfo'",
  "@role='complementary'",
  "@role='form'",
  "@role='dialog'",
  "@role='region'",
].join(" or ");

function id(info: TestInfo): string {
  return testId(info.project.name, relFile(info.config.rootDir, info.file), info.titlePath.slice(1));
}

async function hook(kind: "begin" | "end", test: string) {
  const api = process.env.LOCALCI_API;
  if (!api) return;
  try {
    await fetch(`${api.replace(/\/$/, "")}/hooks/test/${kind}?test=${encodeURIComponent(test)}`, {
      method: "POST",
      headers: process.env.LOCALCI_HOOK_TOKEN ? { Authorization: `Bearer ${process.env.LOCALCI_HOOK_TOKEN}` } : {},
      signal: AbortSignal.timeout(30_000),
    });
  } catch {
    // The core being slow or gone must not fail the test.
  }
}

const strip = (s: string) => s.replace(/\u001b\[[0-9;]*m/g, "");

/** Accessibility tree of the failing locator's nearest landmark, else of the whole body. */
async function ariaFor(page: Page, message: string): Promise<string | null> {
  const opts = { timeout: 2000 };
  const text = locatorTextFromMessage(message);
  const parsed = text ? locatorFromText(page, text) : null;
  let root = page.locator("body");
  let hint = parsed?.hint ?? null;
  if (parsed) {
    try {
      const target = parsed.locator.first();
      if ((await parsed.locator.count()) > 0) {
        const own = await target.evaluate(
          (el) => ({ label: el.getAttribute("aria-label") ?? "", text: (el.textContent ?? "").trim().split("\n")[0].slice(0, 60) }),
          undefined,
          opts,
        );
        hint = own.label || own.text || hint;
        const landmark = target.locator(`xpath=ancestor-or-self::*[${LANDMARKS}][1]`);
        if ((await landmark.count()) > 0) root = landmark;
      }
    } catch {
      // fall through with the body
    }
  }
  try {
    return excerpt(await root.ariaSnapshot(opts), hint);
  } catch {
    return null;
  }
}

function watch(context: BrowserContext, into: { console: string[]; network: NetworkError[] }) {
  context.on("console", (m) => {
    if (m.type() === "error") into.console.push(clip(m.text()));
  });
  context.on("weberror", (e) => into.console.push(clip(String(e.error().message))));
  context.on("requestfailed", (r) => into.network.push({ method: r.method(), url: safeUrl(r.url()), status: 0 }));
  context.on("response", (r) => {
    if (r.status() >= 400) into.network.push({ method: r.request().method(), url: safeUrl(r.url()), status: r.status() });
  });
}

/** `test` and `expect` for suites reporting to localci; the auto fixture needs no setup. */
export const test = base.extend<{ _localci: void; flow: FlowCaller; tenant: Tenant }>({
  // The shard's tenant (see tenant.ts): inert unless the suite uses `isolation: tenant`.
  tenant: [async ({ page, context, request }, use, info) => tenantFixture({ page, context, request }, use, info), { auto: true }],
  // Playwright reads the fixtures a function needs from its destructured first argument.
  flow: async ({ page, context, request, baseURL }, use, info) => flowFixture({ page, context, request, baseURL }, use, info),
  _localci: [
    async ({ context }, use, info) => {
      const seen = { console: [] as string[], network: [] as NetworkError[] };
      watch(context, seen);
      const test = id(info);
      await hook("begin", test);
      const stopCoverage = process.env.LOCALCI_REPORT ? await trackCoverage(context, test) : null;
      await use();
      const covered = stopCoverage ? await stopCoverage() : null;
      if (covered && (covered.files.length || covered.incomplete))
        await info.attach(COVERAGE_ATTACHMENT, { body: JSON.stringify(covered), contentType: "application/json" });
      if (info.status !== info.expectedStatus) {
        const message = strip(info.errors.map((e) => e.message ?? "").join("\n"));
        const page = context.pages().filter((p) => !p.isClosed()).pop();
        const aria = page ? await ariaFor(page, message) : null;
        const ctx: FailureContext = {
          console: seen.console.slice(-MAX_LIST),
          network: seen.network.slice(-MAX_LIST),
          aria,
        };
        await info.attach(CONTEXT_ATTACHMENT, { body: JSON.stringify(ctx), contentType: "application/json" });
        if (aria) {
          const file = info.outputPath("aria.yml");
          fs.writeFileSync(file, aria + "\n");
          await info.attach(ARIA_ATTACHMENT, { path: file, contentType: "text/yaml" });
        }
      }
      await hook("end", test);
    },
    { auto: true },
  ],
});

export { expect };
