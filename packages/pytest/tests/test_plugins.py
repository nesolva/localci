"""Other pytest plugins the suites run with: retries and parallel workers."""
from importlib.metadata import PackageNotFoundError, distribution

import pytest

from conftest import SHOP, assert_valid, run_pytest, write_project


def test_each_rerun_attempt_is_its_own_test_begin_and_end(tmp_path):
    pytest.importorskip("pytest_rerunfailures")
    files = {
        "test_flaky.py": """
            from pathlib import Path
            import localci_pytest as ci

            def test_flaky():
                marker = Path("attempts")
                n = len(marker.read_text()) if marker.exists() else 0
                marker.write_text("x" * (n + 1))
                with ci.step("try"):
                    assert n >= 1
        """
    }
    run = run_pytest(write_project(tmp_path / "acme-shop", files), "--reruns", "2")
    assert run.status == 0
    assert_valid(run.events)
    tid = run.events[1]["test"]
    attempts = [(e["t"], e.get("retry"), e.get("status")) for e in run.events if e["t"].startswith("test.")]
    assert attempts == [("test.begin", 0, None), ("test.end", 0, "failed"), ("test.begin", 1, None), ("test.end", 1, "passed")]
    # a retry's steps must not reuse the first attempt's ids
    assert [e["step"] for e in run.events if e["t"] == "step.begin"] == ["%s:1" % tid, "%s:2" % tid]
    assert run.events[0]["total"] == 1 and run.events[-1]["status"] == "passed"


def test_xdist_workers_report_tests_and_the_controller_reports_the_run(tmp_path):
    pytest.importorskip("xdist")
    run = run_pytest(write_project(tmp_path / "acme-shop", SHOP), "-n", "2")
    assert_valid(run.events)
    kinds = [e["t"] for e in run.events]
    assert kinds[0] == "begin" and kinds.count("begin") == 1 and kinds.count("end") == 1 and kinds[-1] == "end"
    assert run.events[0]["total"] == 9
    began = sorted(e["test"] for e in run.events if e["t"] == "test.begin")
    assert began == sorted(e["test"] for e in run.events if e["t"] == "test.end") and len(set(began)) == 9
    assert [e["status"] for e in run.events if e["t"] == "end"] == ["failed"]


def test_the_installed_entry_point_loads_the_plugin_without_a_flag(tmp_path):
    try:
        points = {e.name: e.value for e in distribution("localci-pytest").entry_points if e.group == "pytest11"}
    except PackageNotFoundError:
        points = {}
    if points != {"localci_pytest": "localci_pytest"}:
        pytest.skip("the package is not installed here")
    run = run_pytest(write_project(tmp_path / "acme-shop", {"test_a.py": "def test_a():\n    pass\n"}), plugin=None)
    assert run.status == 0 and [e["t"] for e in run.events] == ["begin", "test.begin", "test.end", "end"]
    assert_valid(run.events)
