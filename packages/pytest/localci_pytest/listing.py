"""`python -m localci_pytest list [pytest args]`: the suite as NDJSON `{test,file,title,line}`, running nothing."""
from __future__ import annotations

import contextlib
import json
import os
import sys
from typing import Any, List, Optional

import pytest

from .ids import describe


class _Collector:
    def __init__(self) -> None:
        self.items: List[Any] = []
        self.config: Optional[pytest.Config] = None

    def pytest_collection_finish(self, session: pytest.Session) -> None:
        # After every plugin has deselected what it wants to (`-k`, `-m`).
        self.items = list(session.items)
        self.config = session.config


def main(argv: List[str]) -> int:
    if not argv or argv[0] != "list":
        print("usage: python -m localci_pytest list [pytest args]", file=sys.stderr)
        return 2
    # The listing is the whole suite and writes no report, whatever the environment says.
    for name in ("LOCALCI_TESTS", "LOCALCI_REPORT"):
        os.environ.pop(name, None)
    collector = _Collector()
    # Anything pytest prints (collection errors) goes to stderr: stdout is the listing.
    with contextlib.redirect_stdout(sys.stderr):
        code = int(pytest.main(["--collect-only", "-q", *argv[1:]], plugins=[collector]))
    # 5 = no tests collected: an empty suite is a listing with no lines, not an error. Anything else (a test file
    # that does not import, a bad argument) must fail, or its tests would silently vanish from the run.
    if code not in (0, 5) or collector.config is None:
        print("localci_pytest list: pytest exited with status %d" % code, file=sys.stderr)
        return 2
    for item in collector.items:
        d = describe(item, collector.config)
        print(json.dumps({"test": d.id, "file": d.file, "title": d.title, "line": d.line}))
    sys.stdout.flush()
    return 0
