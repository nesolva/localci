"""Test ids must equal the Playwright package's, and `list` and a run must agree on them."""
import json
import os
import subprocess
import sys

import pytest

from conftest import PACKAGE, SHOP, run_pytest, write_project
from localci_pytest.ids import compute_id

CASES = json.loads((PACKAGE.parents[1] / "protocol" / "test-ids.json").read_text(encoding="utf-8"))


@pytest.mark.parametrize("case", CASES, ids=lambda c: c["id"])
def test_ids_match_the_shared_fixture_the_playwright_package_checks(case):
    assert compute_id(case["project"], case["file"], case["titles"]) == case["id"]


def list_ids(project, env=None, args=()):
    base = {k: v for k, v in os.environ.items() if not k.startswith("LOCALCI_")}
    base.update(PYTHONPATH=str(PACKAGE), **(env or {}))
    r = subprocess.run([sys.executable, "-m", "localci_pytest", "list", *args], cwd=project, env=base, capture_output=True, text=True)
    return r, [json.loads(line) for line in r.stdout.splitlines()]


def test_ids_come_from_the_rootdir_name_and_from_localci_project_when_set(tmp_path):
    project = write_project(tmp_path / "acme-orders", {"tests/test_a.py": "class TestA:\n    def test_x(self):\n        pass\n"})
    first = run_pytest(project).events[1]
    assert first["test"] == compute_id("acme-orders", "tests/test_a.py", ["TestA", "test_x"])
    assert first["file"] == "tests/test_a.py" and first["title"] == "TestA › test_x"
    renamed = run_pytest(project, env={"LOCALCI_PROJECT": "orders-api"}).events[1]
    assert renamed["test"] == compute_id("orders-api", "tests/test_a.py", ["TestA", "test_x"])


def test_ids_of_list_and_of_a_run_agree(tmp_path):
    # If they differed, every test would be listed under one id and run under another: all interrupted
    project = write_project(tmp_path / "acme-shop", SHOP)
    run = run_pytest(project)
    listed, tests = list_ids(project)
    assert listed.returncode == 0
    assert {t["test"]: t["title"] for t in tests} == {
        e["test"]: e["title"] for e in run.events if e["t"] == "test.begin"
    }
    assert {t["test"]: t["line"] for t in tests} == {e["test"]: e["line"] for e in run.events if e["t"] == "test.begin"}
