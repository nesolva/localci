import json
import os
import urllib.error
import urllib.request

import pytest


class Client:
    """A minimal JSON client. The base URLs are read when a test runs, not when it is collected: localci lists the
    suite without its stack."""

    def __init__(self, base):
        self.base = base

    def call(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=10) as r:
                return r.status, json.load(r)
        except urllib.error.HTTPError as e:
            return e.code, json.load(e)

    def get(self, path):
        return self.call("GET", path)

    def post(self, path, body):
        return self.call("POST", path, body)


@pytest.fixture
def api():
    yield Client(os.environ["ORDERS_URL"])


@pytest.fixture
def payments():
    yield Client(os.environ["PAYMENTS_URL"])
