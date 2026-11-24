/**
 * Playwright prints the failing locator as source text ("getByRole('button', { name: 'Pay' })").
 * This turns that text back into calls on a page without evaluating it as code: a tiny parser for
 * a chain of allowlisted locator methods whose arguments are literals.
 */
import type { Locator, Page } from "@playwright/test";

const METHODS = new Set([
  "locator",
  "getByRole",
  "getByText",
  "getByLabel",
  "getByPlaceholder",
  "getByTestId",
  "getByAltText",
  "getByTitle",
  "filter",
  "first",
  "last",
  "nth",
]);

type Value = string | number | boolean | null | RegExp | Value[] | { [k: string]: Value };

class Parser {
  private i = 0;
  constructor(private s: string) {}

  chain(): { name: string; args: Value[] }[] {
    const calls: { name: string; args: Value[] }[] = [];
    for (;;) {
      this.ws();
      if (calls.length > 0 && !this.eat(".")) break;
      const name = this.ident();
      if (!name || !METHODS.has(name)) throw new Error(`unsupported call ${name}`);
      this.ws();
      this.expect("(");
      const args: Value[] = [];
      this.ws();
      while (this.peek() !== ")") {
        args.push(this.value());
        this.ws();
        if (!this.eat(",")) break;
        this.ws();
      }
      this.expect(")");
      calls.push({ name, args });
      this.ws();
      if (this.i >= this.s.length) break;
    }
    return calls;
  }

  private value(): Value {
    const c = this.peek();
    if (c === "'" || c === '"' || c === "`") return this.string(c);
    if (c === "/") return this.regex();
    if (c === "{") return this.object();
    if (c === "[") return this.array();
    const word = this.ident(true);
    if (word === "true") return true;
    if (word === "false") return false;
    if (word === "null") return null;
    const n = Number(word);
    if (word !== "" && !Number.isNaN(n)) return n;
    throw new Error(`unsupported value ${word}`);
  }

  private string(q: string): string {
    this.i++;
    let out = "";
    while (this.i < this.s.length && this.s[this.i] !== q) {
      if (this.s[this.i] === "\\") {
        const n = this.s[this.i + 1];
        out += n === "n" ? "\n" : n === "t" ? "\t" : n;
        this.i += 2;
      } else out += this.s[this.i++];
    }
    this.expect(q);
    return out;
  }

  private regex(): RegExp {
    this.i++;
    let body = "";
    while (this.i < this.s.length && this.s[this.i] !== "/") {
      if (this.s[this.i] === "\\") body += this.s[this.i++];
      body += this.s[this.i++];
    }
    this.expect("/");
    let flags = "";
    while (/[a-z]/.test(this.s[this.i] ?? "")) flags += this.s[this.i++];
    return new RegExp(body, flags);
  }

  private object(): Value {
    this.expect("{");
    const out: { [k: string]: Value } = {};
    this.ws();
    while (this.peek() !== "}") {
      const q = this.peek();
      const key = q === "'" || q === '"' ? this.string(q) : this.ident();
      this.ws();
      this.expect(":");
      this.ws();
      out[key] = this.value();
      this.ws();
      if (!this.eat(",")) break;
      this.ws();
    }
    this.expect("}");
    return out;
  }

  private array(): Value {
    this.expect("[");
    const out: Value[] = [];
    this.ws();
    while (this.peek() !== "]") {
      out.push(this.value());
      this.ws();
      if (!this.eat(",")) break;
      this.ws();
    }
    this.expect("]");
    return out;
  }

  private ident(loose = false): string {
    const re = loose ? /[A-Za-z0-9_.\-]/ : /[A-Za-z0-9_$]/;
    const start = this.i;
    while (this.i < this.s.length && re.test(this.s[this.i])) this.i++;
    return this.s.slice(start, this.i);
  }

  private ws() {
    while (/\s/.test(this.s[this.i] ?? "x")) this.i++;
  }
  private peek() {
    return this.s[this.i];
  }
  private eat(c: string) {
    if (this.s[this.i] === c) {
      this.i++;
      return true;
    }
    return false;
  }
  private expect(c: string) {
    if (!this.eat(c)) throw new Error(`expected ${c} at ${this.i}`);
  }
}

export interface ParsedLocator {
  locator: Locator;
  /** The first string argument: what the test was looking for, used to window a long snapshot. */
  hint: string | null;
}

/** Returns null when the text uses anything outside the supported subset. */
export function locatorFromText(page: Page, text: string): ParsedLocator | null {
  try {
    const calls = new Parser(text.trim()).chain();
    if (calls.length === 0) return null;
    let cur: any = page;
    let hint: string | null = null;
    for (const { name, args } of calls) {
      if (hint === null) hint = args.find((a): a is string => typeof a === "string") ?? null;
      cur = cur[name](...args);
    }
    return { locator: cur as Locator, hint };
  } catch {
    return null;
  }
}

/** The locator text Playwright puts in a failure message, if any. */
export function locatorTextFromMessage(message: string): string | null {
  const m = /^\s*(?:Locator:|- waiting for)\s+(.+?)\s*$/m.exec(message);
  return m ? m[1] : null;
}
