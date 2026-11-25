"""Writes protocol v1 events for one pytest process. Registered only when `$LOCALCI_REPORT` is set."""
from __future__ import annotations

import inspect
import json
import os
import re
import sys
import time
from typing import Any, Dict, List, NamedTuple, Optional, Tuple

import pytest

from . import hooks
from .ids import TITLE_SEP, Described, describe

_ANSI = re.compile(r"\x1b\[[0-9;]*m")
MAX_TEXT = 2000

# The recorder of this process, or None when the plugin is inactive. `step()` talks to it.
active: Optional["Recorder"] = None


def _clip(text: str) -> str:
    text = _ANSI.sub("", text)
    return text if len(text) <= MAX_TEXT else text[: MAX_TEXT - 1] + "…"


def _shard() -> int:
    try:
        # 0 is the serial group and reruns; the protocol counts shards from 1.
        return max(1, int(os.environ.get("LOCALCI_SHARD") or 1))
    except ValueError:
        return 1


class _Open(NamedTuple):
    sid: str
    name: str
    kind: str  # "step" for the user's steps, "hook" for fixtures
    began: float


class _Attempt:
    """One run of one test: from `test.begin` to `test.end`."""

    def __init__(self, item: pytest.Item, d: Described, retry: int) -> None:
        self.item = item
        self.d = d
        self.retry = retry
        self.began = time.time()
        self.stack: List[_Open] = []
        self.reports: List[Any] = []
        self.failure: Optional[Dict[str, Any]] = None
        # The exception that last passed through a user step, and the steps it passed through, innermost last.
        self.step_failure: Optional[Tuple[BaseException, List[str]]] = None
        # The exception a fixture's setup raised, and the step to blame.
        self.setup_failure: Optional[Tuple[BaseException, str]] = None
        # (op, repr(left), repr(right)) of the last failed comparison.
        self.compare: Optional[Tuple[str, str, str]] = None


class Recorder:
    def __init__(self, config: pytest.Config, report: str) -> None:
        self.config = config
        try:
            # One `write` per line on an append-only descriptor keeps lines whole with several writers.
            self.fd: Optional[int] = os.open(report, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
        except OSError as e:
            raise pytest.UsageError("localci_pytest: cannot open $LOCALCI_REPORT: %s" % e)
        # An xdist worker reports its tests; the process that started the workers reports the run.
        self.worker = hasattr(config, "workerinput")
        self.attempt: Optional[_Attempt] = None
        self.begun = False
        self._attempts: Dict[str, int] = {}
        self._step_counts: Dict[str, int] = {}

    def close(self) -> None:
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None

    # -- events ------------------------------------------------------------------------------------------------

    def emit(self, t: str, **body: Any) -> None:
        if self.fd is None:
            return
        event: Dict[str, Any] = {"v": 1, "t": t, "ts": int(time.time() * 1000)}
        event.update(body)
        line = json.dumps(event, separators=(",", ":")) + "\n"  # ASCII only: escapes stay valid whatever the locale
        try:
            os.write(self.fd, line.encode("ascii"))
        except OSError:
            pass

    def _warn(self, what: str, e: BaseException) -> None:
        # A bug in reporting must not turn into a failing test.
        print("localci_pytest: %s: %s: %s" % (what, type(e).__name__, e), file=sys.stderr)

    def _begin(self, total: int) -> None:
        if not self.begun:
            self.begun = True
            self.emit("begin", shard=_shard(), total=total)

    # -- collection --------------------------------------------------------------------------------------------

    def pytest_collection_modifyitems(self, session: pytest.Session, config: pytest.Config, items: List[pytest.Item]) -> None:
        path = os.environ.get("LOCALCI_TESTS")
        if not path:
            return
        try:
            with open(path, encoding="utf-8") as f:
                wanted = {line.strip() for line in f if line.strip()}
        except OSError as e:
            # Running everything would be the wrong answer to a shard that was told what to run.
            raise pytest.UsageError("localci_pytest: cannot read $LOCALCI_TESTS: %s" % e)
        keep, drop = [], []
        for item in items:
            (keep if describe(item, config).id in wanted else drop).append(item)
        if drop:
            config.hook.pytest_deselected(items=drop)  # deselected, not skipped
            items[:] = keep

    def pytest_collection_finish(self, session: pytest.Session) -> None:
        if not self.worker and not session.config.option.collectonly:
            self._begin(len(session.items))

    @pytest.hookimpl(optionalhook=True)
    def pytest_xdist_node_collection_finished(self, node: Any, ids: List[str]) -> None:
        # With xdist the process that reports `begin` never collects: it hears the count from its workers.
        self._begin(len(ids))

    # -- a test ------------------------------------------------------------------------------------------------

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_setup(self, item: pytest.Item) -> Any:
        try:
            self._start(item)
        except Exception as e:
            self._warn("test.begin", e)
        yield

    def _start(self, item: pytest.Item) -> None:
        d = describe(item, self.config)
        retry = self._attempts.get(item.nodeid, 0)  # reruns call the setup again for the same item
        self._attempts[item.nodeid] = retry + 1
        self.attempt = _Attempt(item, d, retry)
        self.emit("test.begin", test=d.id, file=d.file, title=d.title, line=d.line, retry=retry)
        hooks.call("begin", d.id)

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_call(self, item: pytest.Item) -> Any:
        if self.attempt is not None:
            self.attempt.compare = None
        yield

    @pytest.hookimpl(tryfirst=True)
    def pytest_assertrepr_compare(self, config: pytest.Config, op: str, left: Any, right: Any) -> None:
        # Never explains anything itself: it only remembers what was compared, for `failure.expected/actual`.
        if self.attempt is not None and op == "==":
            try:
                self.attempt.compare = (op, _clip(repr(left)), _clip(repr(right)))
            except Exception:
                self.attempt.compare = None
        return None

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_makereport(self, item: pytest.Item, call: Any) -> Any:
        outcome = yield
        attempt = self.attempt
        if attempt is None or attempt.item is not item:
            return
        try:
            report = outcome.get_result()
            attempt.reports.append(report)
            if report.failed and attempt.failure is None:
                attempt.failure = self._failure(attempt, call, report)
        except Exception as e:
            self._warn("failure report", e)
        if call.when == "teardown":
            try:
                self._finish(attempt)
            except Exception as e:
                self._warn("test.end", e)
                self.attempt = None

    def _finish(self, attempt: _Attempt) -> None:
        reports = attempt.reports
        if attempt.failure is not None or any(r.failed for r in reports):
            status = "failed"
        elif any(r.skipped and not hasattr(r, "wasxfail") for r in reports):
            status = "skipped"
        else:
            status = "passed"  # an xfail that failed as declared, and a non-strict xpass, are what the suite expects
        body: Dict[str, Any] = {
            "test": attempt.d.id,
            "status": status,
            "ms": round(sum(r.duration for r in reports) * 1000, 3),
            "retry": attempt.retry,
        }
        if status == "failed":
            body["failure"] = attempt.failure or self._bare_failure(attempt)
        self._close_steps(attempt, False)
        hooks.call("end", attempt.d.id)
        self.emit("test.end", **body)
        self.attempt = None

    # -- failures ----------------------------------------------------------------------------------------------

    def _failure(self, attempt: _Attempt, call: Any, report: Any) -> Dict[str, Any]:
        excinfo = call.excinfo
        value = excinfo.value if excinfo is not None else None
        if excinfo is not None:
            try:
                message = excinfo.exconly(tryshort=True)
            except Exception:
                message = type(value).__name__
        else:
            message = str(report.longrepr)  # a strict xpass has no exception
        message = _clip(message)

        step = None
        if value is not None:
            if attempt.step_failure and attempt.step_failure[0] is value and attempt.step_failure[1]:
                step = TITLE_SEP.join(attempt.step_failure[1])
            elif attempt.setup_failure and attempt.setup_failure[0] is value:
                step = attempt.setup_failure[1]
        if not step:
            step = attempt.d.titles[-1] if attempt.d.titles else attempt.d.title

        expected = actual = None
        cmp = attempt.compare
        # The convention is `assert actual == expected`; anything else is not guessed.
        if cmp and isinstance(value, AssertionError) and "==" in message:
            actual, expected = cmp[1], cmp[2]

        return {
            "step": step,
            "message": message,
            "expected": expected,
            "actual": actual,
            "location": self._location(attempt, excinfo),
            "aria": None,
            "console": [],
            "network": [],
            "screenshot": None,
            "trace": None,
        }

    def _bare_failure(self, attempt: _Attempt) -> Dict[str, Any]:
        return {
            "step": attempt.d.title, "message": "The test failed", "expected": None, "actual": None, "location": None,
            "aria": None, "console": [], "network": [], "screenshot": None, "trace": None,
        }

    def _location(self, attempt: _Attempt, excinfo: Any) -> Optional[str]:
        """The failing line in the test's own file, else the deepest line under the rootdir, else the deepest line."""
        entries = []
        if excinfo is not None:
            try:
                entries = list(excinfo.traceback)
            except Exception:
                entries = []
        own = os.path.realpath(str(attempt.item.path))
        root = os.path.realpath(str(self.config.rootpath)) + os.sep
        under_root = None
        for entry in reversed(entries):
            try:
                path = str(entry.path)
                real = os.path.realpath(path)
                where = "%s:%d" % (os.path.abspath(path), entry.lineno + 1)
            except Exception:
                continue
            if real == own:
                return where
            if under_root is None and real.startswith(root):
                under_root = where
        if under_root:
            return under_root
        if entries:
            try:
                return "%s:%d" % (os.path.abspath(str(entries[-1].path)), entries[-1].lineno + 1)
            except Exception:
                pass
        return "%s:%d" % (attempt.item.path, attempt.d.line) if attempt.d.line else str(attempt.item.path)

    # -- steps -------------------------------------------------------------------------------------------------

    def step_begin(self, name: str, kind: str) -> Optional[str]:
        attempt = self.attempt
        if attempt is None:
            return None
        n = self._step_counts.get(attempt.d.id, 0) + 1
        self._step_counts[attempt.d.id] = n  # never reset: a retry's steps get new ids
        sid = "%s:%d" % (attempt.d.id, n)
        parent = attempt.stack[-1].sid if attempt.stack else None
        attempt.stack.append(_Open(sid, name, kind, time.time()))
        self.emit("step.begin", test=attempt.d.id, step=sid, parent=parent, title=name, kind=kind)
        return sid

    def step_end(self, sid: Optional[str], ok: bool, exc: Optional[BaseException] = None) -> None:
        attempt = self.attempt
        if sid is None or attempt is None:
            return
        for i in range(len(attempt.stack) - 1, -1, -1):
            if attempt.stack[i].sid == sid:
                break
        else:
            return  # already closed (the attempt ended inside it)
        if exc is not None and (attempt.step_failure is None or attempt.step_failure[0] is not exc):
            # The first step the final exception leaves is the innermost one: that chain is the failing step.
            attempt.step_failure = (exc, [s.name for s in attempt.stack[: i + 1] if s.kind == "step"])
        begun = attempt.stack[i].began
        del attempt.stack[i:]
        self.emit("step.end", step=sid, ms=round((time.time() - begun) * 1000, 3), ok=ok)

    def _close_steps(self, attempt: _Attempt, ok: bool) -> None:
        while attempt.stack:
            self.step_end(attempt.stack[-1].sid, ok)

    @pytest.hookimpl(hookwrapper=True)
    def pytest_fixture_setup(self, fixturedef: Any, request: Any) -> Any:
        # Only runs when the fixture really executes: a cached one is not set up again.
        name = fixturedef.argname
        func = getattr(fixturedef, "func", None)
        attempt = self.attempt
        # pytest's own fixtures (`tmp_path`, `monkeypatch`, the stand-ins for parametrized arguments) are not the
        # suite's setup: showing them would bury it.
        if attempt is None or str(getattr(func, "__module__", "")).startswith("_pytest."):
            yield
            return
        sid = self.step_begin("setup: %s" % name, "hook")
        torn: Dict[str, Optional[str]] = {"sid": None}
        generator = inspect.isgeneratorfunction(func) or inspect.isasyncgenfunction(func)
        if generator:
            # Finalizers run last in, first out: this one closes the teardown step after the fixture's own code.
            fixturedef.addfinalizer(lambda: self._teardown_end(torn))
        outcome = yield
        error = outcome.excinfo[1] if outcome.excinfo is not None else None
        ok = outcome.excinfo is None or isinstance(error, pytest.skip.Exception)
        if error is not None and not ok:
            attempt.setup_failure = (error, "setup: %s" % name)
        self.step_end(sid, ok)
        if generator and outcome.excinfo is None:
            fixturedef.addfinalizer(lambda: self._teardown_begin(torn, name))  # and this one opens it, before the fixture's

    def _teardown_begin(self, torn: Dict[str, Optional[str]], name: str) -> None:
        try:
            torn["sid"] = self.step_begin("teardown: %s" % name, "hook")
        except Exception as e:
            self._warn("teardown step", e)

    def _teardown_end(self, torn: Dict[str, Optional[str]]) -> None:
        # A teardown error is not visible per fixture: it fails the test's teardown phase, which the failure reports.
        try:
            self.step_end(torn["sid"], True)
        except Exception as e:
            self._warn("teardown step", e)

    # -- the end -----------------------------------------------------------------------------------------------

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(self, session: pytest.Session, exitstatus: Any) -> None:
        attempt = self.attempt
        if attempt is not None:
            # The process was interrupted inside a test: it began and will never end by itself.
            self._close_steps(attempt, False)
            hooks.call("end", attempt.d.id)
            self.emit("test.end", test=attempt.d.id, status="interrupted", ms=round((time.time() - attempt.began) * 1000, 3), retry=attempt.retry)
            self.attempt = None
        if self.worker or self.config.option.collectonly:
            return
        self._begin(0)  # collection failed before it could say how many tests there were
        code = int(exitstatus)
        # 5 = no tests collected: a shard may legitimately have none.
        status = "passed" if code in (0, 5) else "interrupted" if code == 2 else "failed"
        self.emit("end", status=status)
