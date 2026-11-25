"""The service under test: a tiny orders API over HTTP. It prices an order and charges a payments backend.

Standard library only. `python -m orders.service` listens on $PORT and reads the payments backend from $PAYMENTS_URL.
"""
import json
import os
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CATALOG = {"mug": 900, "poster": 1500, "sticker": 250}  # price in cents
ORDERS = {}
LOCK = threading.Lock()
# Every request waits a little, so the sample is long enough to show sharding.
LATENCY = int(os.environ.get("LATENCY_MS", "0")) / 1000
# Set ACME_BREAK=1 to give the sample a bug: the bulk discount starts at two items instead of ten.
BULK_FROM = 2 if os.environ.get("ACME_BREAK") else 10


def price(sku, qty):
    total = CATALOG[sku] * qty
    return total * 9 // 10 if qty >= BULK_FROM else total  # 10% off in bulk


def charge(amount, card):
    request = urllib.request.Request(
        os.environ["PAYMENTS_URL"] + "/charges",
        data=json.dumps({"amount": amount, "card": card}).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)
    except OSError:
        return 502, {"error": "payments unreachable"}


class Handler(BaseHTTPRequestHandler):
    def send(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        time.sleep(LATENCY)
        if self.path == "/healthz":
            return self.send(200, {"ok": True})
        order = ORDERS.get(self.path.removeprefix("/orders/")) if self.path.startswith("/orders/") else None
        self.send(200, order) if order else self.send(404, {"error": "no such order"})

    def do_POST(self):
        time.sleep(LATENCY)
        if self.path != "/orders":
            return self.send(404, {"error": "not found"})
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        sku, qty = body.get("sku"), body.get("qty")
        if sku not in CATALOG:
            return self.send(404, {"error": "unknown sku"})
        if not isinstance(qty, int) or not 1 <= qty <= 100:
            return self.send(422, {"error": "qty must be between 1 and 100"})
        total = price(sku, qty)
        status, paid = charge(total, body.get("card", "tok_ok"))
        if status == 502:
            return self.send(502, paid)
        if status != 201:
            return self.send(402, {"error": "payment declined"})
        with LOCK:
            order = {"id": "o%d" % (len(ORDERS) + 1), "sku": sku, "qty": qty, "total": total, "charge": paid["id"]}
            ORDERS[order["id"]] = order
        self.send(201, order)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
