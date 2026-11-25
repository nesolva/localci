"""Helpers: every test runs pytest in a throwaway project, the way localci runs a shard."""
import json
import os
import re
import socket
import subprocess
import sys
import textwrap
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any, Dict, List, NamedTuple, Optional

import pytest
from jsonschema import Draft7Validator

PACKAGE = Path(__file__).resolve().parents[1]
SCHEMA = json.loads((PACKAGE.parents[1] / "protocol" / "v1.schema.json").read_text(encoding="utf-8"))
VALIDATOR = Draft7Validator(SCHEMA)


class Run(NamedTuple):
    status: int
    events: List[Dict[str, Any]]
    out: str
    report: Path


def write_project(root: Path, files: Dict[str, str]) -> Path:
    root.mkdir(parents=True, exist_ok=True)
    (root / "pytest.ini").write_text("[pytest]\n", encoding="utf-8")
    for name, body in files.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(textwrap.dedent(body), encoding="utf-8")
    return root


def read_events(report: Path) -> List[Dict[str, Any]]:
    if not report.exists():
        return []
    return [json.loads(line) for line in report.read_text(encoding="utf-8").splitlines() if line]


def run_pytest(
    project: Path, *args: str, env: Optional[Dict[str, Optional[str]]] = None, report: bool = True, plugin: Optional[bool] = True
) -> Run:
    """Runs `pytest -p localci_pytest` in `project` as localci would; `env` values of None are removed."""
    out = project.parent / (project.name + "-report.ndjson")
    out.unlink(missing_ok=True)
    base = {k: v for k, v in os.environ.items() if not k.startswith(("LOCALCI_", "PYTEST_"))}
    base["PYTHONPATH"] = str(PACKAGE)
    if report:
        base["LOCALCI_REPORT"] = str(out)
    for key, value in (env or {}).items():
        if value is None:
            base.pop(key, None)
        else:
            base[key] = value
    # plugin=None: no flag at all, the way a project that installed the package runs it
    flag = [] if plugin is None else ["-p", "localci_pytest" if plugin else "no:localci_pytest"]
    r = subprocess.run(
        [sys.executable, "-m", "pytest", *flag, "-p", "no:cacheprovider", *args],
        cwd=project, env=base, capture_output=True, text=True, timeout=120,
    )
    return Run(r.returncode, read_events(Path(base.get("LOCALCI_REPORT", out))), r.stdout + r.stderr, out)


def summary(out: str) -> str:
    """pytest's last line without the time or the warnings: `2 failed, 4 passed`.

    Warnings are left out because pytest 7.0 warns about the finder module of an editable install (a throwaway
    venv here), which says nothing about the plugin.
    """
    line = re.search(r"=+ (.*?) in [\d.]+s", out.strip().splitlines()[-1]).group(1)
    return re.sub(r", \d+ warnings?", "", line)


def assert_valid(events: List[Dict[str, Any]]) -> None:
    assert events, "the report is empty"
    for e in events:
        errors = [err.message for err in VALIDATOR.iter_errors(e)]
        assert not errors, "%s: %s" % (json.dumps(e)[:300], errors)


def by_title(events: List[Dict[str, Any]]) -> Dict[str, Dict[str, Any]]:
    """test.end events by the title their test.begin gave."""
    titles = {e["test"]: e["title"] for e in events if e["t"] == "test.begin"}
    return {titles[e["test"]]: e for e in events if e["t"] == "test.end"}


class HookServer:
    """A stand-in for localci's API: records the calls and answers `status`."""

    def __init__(self, status: int = 204) -> None:
        self.calls: List[Dict[str, Optional[str]]] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:  # noqa: N802
                outer.calls.append({"path": self.path, "auth": self.headers.get("Authorization")})
                self.send_response(status)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *args: Any) -> None:
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = "http://127.0.0.1:%d" % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def hook_server():
    servers: List[HookServer] = []

    def make(status: int = 204) -> HookServer:
        servers.append(HookServer(status))
        return servers[-1]

    yield make
    for s in servers:
        s.close()


def dead_url() -> str:
    """A local address nothing listens on."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return "http://127.0.0.1:%d" % port


SHOP = {
    "test_shop.py": '''
        import pytest
        import localci_pytest as ci

        @pytest.fixture
        def cart():
            with ci.step("open cart"):
                pass
            yield []

        @pytest.fixture
        def boom():
            raise RuntimeError("cannot reach payments")

        class TestCheckout:
            def test_pays(self, cart):
                with ci.step("pay"):
                    pass

            def test_total_mismatch(self, cart):
                with ci.step("checkout"):
                    with ci.step("compute total"):
                        total = 17
                        assert total == 27

            class TestCoupons:
                def test_expired(self):
                    pass

        @pytest.mark.parametrize("qty,note", [(1, "a-b"), (2, "x y")])
        def test_prices(qty, note):
            pass

        def test_skipped():
            pytest.skip("not today")

        @pytest.mark.xfail(reason="known bug")
        def test_known_bug():
            assert False

        def test_fixture_error(boom):
            pass

        def test_plain_error():
            raise ValueError("\\x1b[31mred\\x1b[0m " + "x" * 3000)
    ''',
}
