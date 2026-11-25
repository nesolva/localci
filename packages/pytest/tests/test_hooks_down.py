"""localci's API being down, slow or failing never fails a test; when it is up, it hears about every test."""
import time

from conftest import assert_valid, dead_url, run_pytest, write_project
from localci_pytest import hooks

TWO = {"test_a.py": "def test_a():\n    pass\n\ndef test_b():\n    assert 1 + 1 == 2\n"}


def assert_clean(run):
    assert run.status == 0, run.out
    assert_valid(run.events)
    assert [e["status"] for e in run.events if e["t"] == "test.end"] == ["passed", "passed"]
    assert run.events[-1] == {**run.events[-1], "t": "end", "status": "passed"}


def test_hooks_down_connection_refused(tmp_path):
    run = run_pytest(write_project(tmp_path / "acme-shop", TWO), env={"LOCALCI_API": dead_url(), "LOCALCI_HOOK_TOKEN": "t"})
    assert_clean(run)


def test_hooks_down_server_answers_500(tmp_path, hook_server):
    server = hook_server(500)
    run = run_pytest(write_project(tmp_path / "acme-shop", TWO), env={"LOCALCI_API": server.url})
    assert_clean(run)
    assert len(server.calls) == 4  # it was asked, and its answer changed nothing


def test_hooks_down_a_server_that_never_answers_is_given_up_on(monkeypatch):
    import socket

    silent = socket.socket()
    silent.bind(("127.0.0.1", 0))
    silent.listen(1)  # accepts the connection, never answers
    monkeypatch.setenv("LOCALCI_API", "http://127.0.0.1:%d" % silent.getsockname()[1])
    monkeypatch.setattr(hooks, "TIMEOUT", 0.3)
    started = time.time()
    hooks.call("begin", "abc")
    silent.close()
    assert time.time() - started < 5


def test_hooks_are_called_for_every_test_with_the_id_and_the_token(tmp_path, hook_server):
    server = hook_server()
    run = run_pytest(write_project(tmp_path / "acme-shop", TWO), env={"LOCALCI_API": server.url + "/", "LOCALCI_HOOK_TOKEN": "s3cret"})
    ids = [e["test"] for e in run.events if e["t"] == "test.begin"]
    assert [c["path"] for c in server.calls] == [
        "/hooks/test/begin?test=%s" % ids[0], "/hooks/test/end?test=%s" % ids[0],
        "/hooks/test/begin?test=%s" % ids[1], "/hooks/test/end?test=%s" % ids[1],
    ]
    assert {c["auth"] for c in server.calls} == {"Bearer s3cret"}
