"""What a run writes: every event is valid protocol v1 and says what happened."""
import textwrap

import pytest

from conftest import SHOP, assert_valid, by_title, run_pytest, write_project


@pytest.fixture(scope="module")
def shop(tmp_path_factory):
    return run_pytest(write_project(tmp_path_factory.mktemp("shop") / "acme-shop", SHOP))


def test_every_event_validates_and_the_report_is_complete(shop):
    assert shop.status == 1  # the suite has failures: pytest says so too
    assert_valid(shop.events)
    assert shop.events[0]["t"] == "begin" and shop.events[-1]["t"] == "end"
    assert shop.events[-1]["status"] == "failed"
    begun = [e["test"] for e in shop.events if e["t"] == "test.begin"]
    ended = [e["test"] for e in shop.events if e["t"] == "test.end"]
    assert begun == ended and len(begun) == shop.events[0]["total"] == 9
    began = [e["step"] for e in shop.events if e["t"] == "step.begin"]
    ended_steps = [e["step"] for e in shop.events if e["t"] == "step.end"]
    assert began and sorted(began) == sorted(ended_steps) and len(set(began)) == len(began)


def test_statuses_follow_what_the_suite_expects(shop):
    status = {title: e["status"] for title, e in by_title(shop.events).items()}
    assert status == {
        "TestCheckout › test_pays": "passed",
        "TestCheckout › test_total_mismatch": "failed",
        "TestCheckout › TestCoupons › test_expired": "passed",
        "test_prices[1-a-b]": "passed",
        "test_prices[2-x y]": "passed",
        "test_skipped": "skipped",
        "test_known_bug": "passed",  # failing as declared is what the suite expects
        "test_fixture_error": "failed",
        "test_plain_error": "failed",
    }


def test_a_failed_comparison_names_the_step_expected_actual_and_line(shop):
    f = by_title(shop.events)["TestCheckout › test_total_mismatch"]["failure"]
    assert f["step"] == "checkout › compute total"
    assert (f["actual"], f["expected"]) == ("17", "27")
    lines = textwrap.dedent(SHOP["test_shop.py"]).splitlines()
    line = next(n for n, text in enumerate(lines, 1) if text.strip() == "assert total == 27")
    assert f["location"].startswith("/") and f["location"].endswith("test_shop.py:%d" % line)
    assert "17 == 27" in f["message"]
    assert (f["console"], f["network"]) == ([], [])
    assert (f["aria"], f["screenshot"], f["trace"]) == (None, None, None)


def test_a_failure_that_is_not_a_comparison_has_no_expected_actual_and_a_clean_message(shop):
    f = by_title(shop.events)["test_plain_error"]["failure"]
    assert (f["expected"], f["actual"]) == (None, None)
    assert f["step"] == "test_plain_error"
    assert "\x1b" not in f["message"] and len(f["message"]) <= 2000 and f["message"].endswith("…")


def test_a_fixture_that_fails_to_set_up_is_the_failing_step(shop):
    ended = by_title(shop.events)["test_fixture_error"]
    assert ended["failure"]["step"] == "setup: boom"
    assert "cannot reach payments" in ended["failure"]["message"]
    begin = next(e for e in shop.events if e["t"] == "step.begin" and e["title"] == "setup: boom")
    end = next(e for e in shop.events if e["t"] == "step.end" and e["step"] == begin["step"])
    assert begin["kind"] == "hook" and end["ok"] is False


def test_steps_nest_and_fixtures_show_their_setup_and_teardown(shop):
    tid = next(e["test"] for e in shop.events if e["t"] == "test.begin" and e["title"].endswith("test_total_mismatch"))
    begins = [e for e in shop.events if e["t"] == "step.begin" and e["test"] == tid]
    assert [(e["title"], e["kind"]) for e in begins] == [
        ("setup: cart", "hook"), ("open cart", "step"), ("checkout", "step"), ("compute total", "step"), ("teardown: cart", "hook"),
    ]
    assert [e["step"] for e in begins] == ["%s:%d" % (tid, n) for n in range(1, 6)]
    parent = {e["title"]: e["parent"] for e in begins}
    assert parent["setup: cart"] is None and parent["open cart"] == begins[0]["step"]
    assert parent["checkout"] is None and parent["compute total"] == begins[2]["step"]
    ok = {e["step"]: e["ok"] for e in shop.events if e["t"] == "step.end"}
    assert [ok[e["step"]] for e in begins] == [True, True, False, False, True]
    # pytest's own fixtures and a parametrized argument are not steps of the suite
    assert not any(e["title"].startswith("setup: ") and e["title"] in ("setup: qty", "setup: note") for e in shop.events if e["t"] == "step.begin")


def test_a_collection_error_still_ends_the_report(tmp_path):
    # localci ignores the exit code once `end` is there: without it a broken import would look like a clean run
    run = run_pytest(write_project(tmp_path / "acme-shop", {"test_broken.py": "import nothing_here_exists\n"}))
    assert run.status != 0
    assert_valid(run.events)
    assert [e["t"] for e in run.events] == ["begin", "end"]
    assert (run.events[0]["total"], run.events[1]["status"]) == (0, "interrupted")


def test_an_interrupted_test_is_closed_as_interrupted(tmp_path):
    files = {"test_stop.py": "import pytest\ndef test_a():\n    pytest.exit('stop here')\ndef test_b():\n    pass\n"}
    run = run_pytest(write_project(tmp_path / "acme-shop", files))
    assert_valid(run.events)
    assert [e["status"] for e in run.events if e["t"] == "test.end"] == ["interrupted"]
    assert run.events[-1] == {**run.events[-1], "t": "end", "status": "interrupted"}


def test_the_shard_of_the_serial_group_is_reported_as_one(tmp_path):
    # LOCALCI_SHARD is 0 for the serial group and for reruns, the protocol counts from 1
    project = write_project(tmp_path / "acme-shop", {"test_a.py": "def test_a():\n    pass\n"})
    assert run_pytest(project, env={"LOCALCI_SHARD": "0"}).events[0]["shard"] == 1
    assert run_pytest(project, env={"LOCALCI_SHARD": "3"}).events[0]["shard"] == 3
