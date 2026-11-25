"""The payments backend, mocked: approves a charge unless the card or the amount says otherwise.

`python -m fakes.payments` listens on $PORT. `GET /charges` lists what it approved, so a test can check what the
service under test really asked for.
"""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CHARGES = []
LIMIT = 100_000  # cents


class Handler(BaseHTTPRequestHandler):
    def send(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.send(200, CHARGES if self.path == "/charges" else {"ok": True})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if body.get("card") == "tok_declined" or body.get("amount", 0) > LIMIT:
            return self.send(402, {"status": "declined"})
        charge = {"id": "ch_%d" % (len(CHARGES) + 1), "status": "approved", "amount": body["amount"]}
        CHARGES.append(charge)
        self.send(201, charge)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
