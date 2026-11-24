// Acme Shop: a small multi-tenant shop used as the example suite for localci.
// No dependencies. State is one JSON file ($DB_PATH), so cloning a database is a file copy.
//
//   PORT=4310 DB_PATH=./data/db.json node server.js
//
// Behaviour toggles, each used to exercise one part of localci:
//   ACME_BREAK=1   the cart total ignores quantities (a failing assertion)
//   ACME_FLAKY=1   adding to the cart silently fails half of the time
//   ACME_RENAME=1  the Pay button is called "Place order"
//   ACME_500=1     payments answer 500
//   ACME_ORDER_500=1  an order's receipt page answers 500 (a page no spec in tests/ visits, for the exploratory agent to find)
//   SLOW_MS=n      every page waits n ms before answering
"use strict";
const http = require("node:http");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const crypto = require("node:crypto");

const PORT = Number(process.env.PORT || 4310);
const DB_PATH = path.resolve(process.env.DB_PATH || path.join(__dirname, "data", "db.json"));
// Shared by every instance on the machine on purpose: global state outside the database.
const GLOBAL_SETTINGS = process.env.ACME_GLOBAL_SETTINGS || path.join(os.tmpdir(), "acme-shop-global-settings.json");
const flag = (name) => process.env[name] === "1";
const SLOW_MS = Number(process.env.SLOW_MS || 0);

const CATALOG = [
  { id: "widget", name: "Widget", cents: 1000 },
  { id: "gadget", name: "Gadget", cents: 2500 },
  { id: "gizmo", name: "Gizmo", cents: 750 },
];

// ---------- storage ----------
let db = { tenants: {} };
try {
  db = JSON.parse(fs.readFileSync(DB_PATH, "utf8"));
} catch {}
// The database file can be replaced under the server (localci restores a checkpoint by dropping and cloning it).
// The server notices by the file's identity and reloads; its own writes are recorded so they never reload.
const fileId = () => {
  try {
    const st = fs.statSync(DB_PATH);
    return `${st.ino}:${st.size}:${st.mtimeMs}`;
  } catch {
    return "";
  }
};
let seen = fileId();
function reload() {
  const now = fileId();
  if (now === seen) return;
  seen = now;
  try {
    db = JSON.parse(fs.readFileSync(DB_PATH, "utf8"));
  } catch {
    db = { tenants: {} };
  }
}
function save() {
  fs.mkdirSync(path.dirname(DB_PATH), { recursive: true });
  const tmp = `${DB_PATH}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(db));
  fs.renameSync(tmp, DB_PATH);
  seen = fileId();
}
function readGlobal() {
  try {
    return { taxPercent: 0, ...JSON.parse(fs.readFileSync(GLOBAL_SETTINGS, "utf8")) };
  } catch {
    return { taxPercent: 0 };
  }
}

// ---------- domain ----------
const money = (c) => `$${(c / 100).toFixed(2)}`;
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const slugify = (s) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "shop";

function createTenant(name) {
  let slug = slugify(name);
  for (let n = 2; db.tenants[slug]; n++) slug = `${slugify(name)}-${n}`;
  db.tenants[slug] = {
    name,
    users: [{ name: "demo", password: "demo123" }],
    carts: {},
    orders: [],
    sessions: {},
  };
  save();
  return slug;
}

function cartLines(t, user) {
  return (t.carts[user] || []).map((l) => {
    const p = CATALOG.find((x) => x.id === l.product);
    return { ...l, name: p.name, cents: p.cents, lineCents: p.cents * l.qty };
  });
}
function cartTotal(t, user) {
  const lines = cartLines(t, user);
  // ACME_BREAK: quantities are ignored once a line has more than one.
  const sub = lines.reduce((n, l) => n + (flag("ACME_BREAK") ? l.cents : l.lineCents), 0);
  return Math.round(sub * (1 + readGlobal().taxPercent / 100));
}
const cartCount = (t, user) => (t.carts[user] || []).reduce((n, l) => n + l.qty, 0);

// ---------- http helpers ----------
function body(req) {
  return new Promise((resolve) => {
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", () => resolve(raw));
  });
}
async function json(req) {
  try {
    return JSON.parse((await body(req)) || "{}");
  } catch {
    return {};
  }
}
function send(res, status, type, text, headers = {}) {
  res.writeHead(status, { "content-type": type, ...headers });
  res.end(text);
}
const sendJSON = (res, status, obj) => send(res, status, "application/json", JSON.stringify(obj));
const redirect = (res, to, headers = {}) => send(res, 302, "text/plain", "", { location: to, ...headers });

function sessionUser(req, slug, t) {
  const m = /(?:^|; )sid=([^;]+)/.exec(req.headers.cookie || "");
  return m && t.sessions[m[1]] ? t.sessions[m[1]] : null;
}

// ---------- html ----------
function layout(slug, t, user, title, main) {
  const base = `/t/${slug}`;
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${esc(title)} · ${esc(t ? t.name : "Acme Shop")}</title>
<style>body{font:16px system-ui;margin:0}header,main{max-width:720px;margin:0 auto;padding:12px 16px}nav a{margin-right:14px}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:6px 4px;border-bottom:1px solid #ddd}[role=alert]{color:#b00020}</style></head><body>
<header><nav aria-label="Main"><a href="${base}/">Shop</a><a href="${base}/cart">Cart (<span data-testid="cart-count">${t && user ? cartCount(t, user) : 0}</span>)</a>
<a href="${base}/orders">Orders</a><a href="${base}/settings">Settings</a>${
    user ? `<span data-testid="whoami">Signed in as ${esc(user)}</span> <a href="${base}/logout">Sign out</a>` : `<a href="${base}/login">Sign in</a>`
  }</nav></header><main>${main}</main></body></html>`;
}

function catalogPage(slug, t, user) {
  const rows = CATALOG.map(
    (p) => `<tr><td>${esc(p.name)}</td><td>${money(p.cents)}</td><td><button type="button" data-add="${p.id}">Add ${esc(p.name)} to cart</button></td></tr>`,
  ).join("");
  return layout(slug, t, user, "Catalog", `<h1>${esc(t.name)}</h1><table aria-label="Products"><tbody>${rows}</tbody></table><p role="status" data-testid="notice"></p>
<script>
document.querySelectorAll('[data-add]').forEach((b) => b.addEventListener('click', async () => {
  const r = await fetch('/t/${slug}/api/cart', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ product: b.dataset.add, delta: 1 }) });
  if (r.status === 401) { location.href = '/t/${slug}/login?next=/t/${slug}/'; return; }
  const j = await r.json();
  document.querySelector('[data-testid=cart-count]').textContent = j.count;
  document.querySelector('[data-testid=notice]').textContent = 'Added ' + b.dataset.add;
}));
</script>`);
}

function cartPage(slug, t, user) {
  const lines = cartLines(t, user);
  const rows = lines
    .map(
      (l) => `<tr data-line-cents="${l.lineCents}"><td>${esc(l.name)}</td><td>${money(l.cents)}</td><td>
<button type="button" data-delta="-1" data-product="${l.product}" aria-label="Fewer ${esc(l.name)}">−</button> <span data-testid="qty-${l.product}">${l.qty}</span>
<button type="button" data-delta="1" data-product="${l.product}" aria-label="More ${esc(l.name)}">+</button></td><td data-testid="line-${l.product}">${money(l.lineCents)}</td>
<td><button type="button" data-remove="${l.product}">Remove ${esc(l.name)}</button></td></tr>`,
    )
    .join("");
  const tax = readGlobal().taxPercent;
  const main = lines.length
    ? `<h1>Your cart</h1><table aria-label="Cart lines"><tbody>${rows}</tbody></table>${tax ? `<p data-testid="tax">Tax ${tax}% included</p>` : ""}
<p data-testid="cart-total" data-total-cents="${cartTotal(t, user)}">Total: ${money(cartTotal(t, user))}</p><p><a href="/t/${slug}/checkout">Checkout</a></p>`
    : `<h1>Your cart</h1><p data-testid="empty">Your cart is empty.</p>`;
  return layout(slug, t, user, "Cart", `${main}
<script>
async function change(product, delta) {
  await fetch('/t/${slug}/api/cart', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ product, delta }) });
  location.reload();
}
document.querySelectorAll('[data-delta]').forEach((b) => b.addEventListener('click', () => change(b.dataset.product, Number(b.dataset.delta))));
document.querySelectorAll('[data-remove]').forEach((b) => b.addEventListener('click', () => change(b.dataset.remove, -1000)));
// The page checks the server's total against its own lines and reports a mismatch.
const total = document.querySelector('[data-total-cents]');
if (total) {
  const lines = [...document.querySelectorAll('[data-line-cents]')].reduce((n, r) => n + Number(r.dataset.lineCents), 0);
  const taxed = Math.round(lines * (1 + ${tax} / 100));
  if (taxed !== Number(total.dataset.totalCents)) {
    console.error('cart total mismatch: lines add up to ' + taxed + ' but the server says ' + total.dataset.totalCents);
    fetch('/api/mismatch', { method: 'POST' });
  }
}
</script>`);
}

function checkoutPage(slug, t, user) {
  const label = flag("ACME_RENAME") ? "Place order" : "Pay";
  return layout(slug, t, user, "Checkout", `<h1>Checkout</h1><form id="pay" aria-label="Payment"><label>Address <input name="address" required></label>
<p data-testid="checkout-total">Total: ${money(cartTotal(t, user))}</p><button type="submit">${label}</button></form><p role="alert" data-testid="pay-error" hidden></p>
<script>
document.getElementById('pay').addEventListener('submit', async (e) => {
  e.preventDefault();
  const r = await fetch('/t/${slug}/api/pay', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ address: e.target.address.value }) });
  const j = await r.json();
  if (r.ok) location.href = '/t/${slug}/orders/' + j.order;
  else { const el = document.querySelector('[data-testid=pay-error]'); el.hidden = false; el.textContent = 'Payment failed, try again.'; }
});
</script>`);
}

function ordersPage(slug, t, user) {
  const rows = t.orders
    .filter((o) => o.user === user)
    .map((o) => `<tr><td><a href="/t/${slug}/orders/${o.id}">Order ${o.id}</a></td><td>${money(o.cents)}</td></tr>`)
    .join("");
  return layout(slug, t, user, "Orders", `<h1>Orders</h1>${rows ? `<table aria-label="Orders"><tbody>${rows}</tbody></table>` : `<p data-testid="no-orders">No orders yet.</p>`}`);
}

function settingsPage(slug, t, user) {
  const g = readGlobal();
  return layout(slug, t, user, "Settings", `<h1>Settings</h1><form id="s" aria-label="Global settings"><label>Tax rate (%) <input name="tax" type="number" value="${g.taxPercent}"></label>
<button type="submit">Save settings</button></form><p role="status" data-testid="saved"></p>
<script>
document.getElementById('s').addEventListener('submit', async (e) => {
  e.preventDefault();
  await fetch('/api/global-settings', { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ taxPercent: Number(e.target.tax.value) }) });
  document.querySelector('[data-testid=saved]').textContent = 'Settings saved';
});
</script>`);
}

function loginPage(slug, t, error, next) {
  return layout(slug, t, null, "Sign in", `<h1>Sign in</h1>${error ? `<p role="alert">${esc(error)}</p>` : ""}<form method="post" action="/t/${slug}/login" aria-label="Sign in">
<input type="hidden" name="next" value="${esc(next || "")}"><label>Username <input name="username"></label> <label>Password <input name="password" type="password"></label>
<button type="submit">Sign in</button></form>`);
}

// ---------- routing ----------
const home = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Acme Shop</title></head><body><main><h1>Acme Shop</h1>
<form id="new" aria-label="Create a shop"><label>Shop name <input name="name" required></label> <button type="submit">Create shop</button></form>
<script>document.getElementById('new').addEventListener('submit', async (e) => { e.preventDefault();
const r = await fetch('/api/tenants', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name: e.target.name.value }) });
location.href = (await r.json()).url; });</script></main></body></html>`;

async function handle(req, res) {
  reload();
  const url = new URL(req.url, "http://x");
  const p = url.pathname;
  if (p === "/healthz") return send(res, 200, "text/plain", "ok");
  if (p === "/" && req.method === "GET") return send(res, 200, "text/html", home);
  if (p === "/api/tenants" && req.method === "POST") {
    const { name } = await json(req);
    if (!name) return sendJSON(res, 400, { error: "name required" });
    const slug = createTenant(String(name));
    return sendJSON(res, 201, { slug, url: `/t/${slug}/`, user: "demo", password: "demo123" });
  }
  if (p === "/api/mismatch" && req.method === "POST") return sendJSON(res, 422, { error: "cart total mismatch recorded" });
  if (p === "/api/global-settings") {
    if (req.method === "PUT") {
      const { taxPercent } = await json(req);
      fs.writeFileSync(GLOBAL_SETTINGS, JSON.stringify({ taxPercent: Number(taxPercent) || 0 }));
    }
    return sendJSON(res, 200, readGlobal());
  }

  const m = /^\/t\/([^/]+)(\/.*)?$/.exec(p);
  const t = m && db.tenants[m[1]];
  if (!t) return send(res, 404, "text/plain", "not found");
  const slug = m[1];
  const rest = m[2] || "/";
  const user = sessionUser(req, slug, t);
  const base = `/t/${slug}`;
  const html = async (text) => {
    if (SLOW_MS) await new Promise((r) => setTimeout(r, SLOW_MS));
    send(res, 200, "text/html", text);
  };
  const needLogin = () => redirect(res, `${base}/login?next=${encodeURIComponent(p)}`);

  if (rest === "/" && req.method === "GET") return html(catalogPage(slug, t, user));
  if (rest === "/login" && req.method === "GET") return html(loginPage(slug, t, null, url.searchParams.get("next")));
  if (rest === "/login" && req.method === "POST") {
    const f = new URLSearchParams(await body(req));
    const u = t.users.find((x) => x.name === f.get("username") && x.password === f.get("password"));
    if (!u) return send(res, 401, "text/html", loginPage(slug, t, "Wrong username or password", f.get("next")));
    const sid = crypto.randomBytes(12).toString("hex");
    t.sessions[sid] = u.name;
    save();
    const next = f.get("next");
    return redirect(res, next && next.startsWith(base) ? next : `${base}/`, { "set-cookie": `sid=${sid}; Path=${base}; HttpOnly; SameSite=Lax` });
  }
  if (rest === "/logout") return redirect(res, `${base}/`, { "set-cookie": `sid=; Path=${base}; Max-Age=0` });
  if (rest === "/settings") return html(settingsPage(slug, t, user));

  // Test support for `isolation: tenant`: the tests of one shard share its tenant, and each starts from a clean shop.
  if (rest === "/api/reset" && req.method === "POST") {
    t.carts = {};
    t.orders = [];
    t.sessions = {};
    save();
    return sendJSON(res, 200, { reset: true });
  }
  if (rest === "/api/me" && req.method === "GET") return user ? sendJSON(res, 200, { user }) : sendJSON(res, 401, { error: "sign in first" });
  if (rest === "/api/cart" && req.method === "POST") {
    if (!user) return sendJSON(res, 401, { error: "sign in first" });
    const { product, delta } = await json(req);
    if (!CATALOG.some((x) => x.id === product)) return sendJSON(res, 400, { error: "unknown product" });
    const skip = flag("ACME_FLAKY") && Math.random() < 0.5;
    if (!skip) {
      const cart = (t.carts[user] = t.carts[user] || []);
      const line = cart.find((l) => l.product === product);
      if (line) line.qty += Number(delta) || 0;
      else cart.push({ product, qty: Math.max(1, Number(delta) || 1) });
      t.carts[user] = cart.filter((l) => l.qty > 0);
      save();
    }
    return sendJSON(res, 200, { count: cartCount(t, user) });
  }
  if (rest === "/api/pay" && req.method === "POST") {
    if (!user) return sendJSON(res, 401, { error: "sign in first" });
    if (flag("ACME_500")) return sendJSON(res, 500, { error: "payment provider unavailable" });
    const cents = cartTotal(t, user);
    if (!cents) return sendJSON(res, 400, { error: "cart is empty" });
    const id = String(t.orders.length + 1);
    t.orders.push({ id, user, cents, lines: cartLines(t, user) });
    t.carts[user] = [];
    save();
    return sendJSON(res, 201, { order: id });
  }

  if (["/cart", "/checkout", "/orders"].includes(rest) || /^\/orders\/\w+(\/receipt)?$/.test(rest)) {
    if (!user) return needLogin();
    if (rest === "/cart") return html(cartPage(slug, t, user));
    if (rest === "/checkout") return html(checkoutPage(slug, t, user));
    if (rest === "/orders") return html(ordersPage(slug, t, user));
    const o = t.orders.find((x) => x.id === rest.split("/")[2] && x.user === user);
    if (!o) return send(res, 404, "text/plain", "not found");
    if (rest.endsWith("/receipt")) {
      if (flag("ACME_ORDER_500")) return send(res, 500, "text/plain", "receipt unavailable");
      const items = o.lines.map((l) => `<li>${esc(l.name)} × ${l.qty}</li>`).join("");
      return html(layout(slug, t, user, `Receipt ${o.id}`, `<h1>Receipt</h1><ul>${items}</ul><p data-testid="receipt-total">Paid ${money(o.cents)}</p>`));
    }
    return html(layout(slug, t, user, `Order ${o.id}`, `<h1>Thank you</h1><p data-testid="order-id">Order ${o.id}</p><p data-testid="order-total">Paid ${money(o.cents)}</p><p><a href="/t/${slug}/orders/${o.id}/receipt">Receipt</a></p>`));
  }
  return send(res, 404, "text/plain", "not found");
}

http
  .createServer((req, res) => handle(req, res).catch((e) => send(res, 500, "text/plain", String(e))))
  .listen(PORT, "127.0.0.1", () => console.log(`acme-shop on http://127.0.0.1:${PORT} (db ${DB_PATH})`));
