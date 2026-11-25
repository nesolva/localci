"""localci's pytest plugin: reports a pytest run to localci with protocol v1.

Load it with `pytest -p localci_pytest` (or let the `pytest11` entry point do it). It only does anything when
`$LOCALCI_REPORT` is set, which localci does for every shard: a plain `pytest` run is unchanged.
"""
from __future__ import annotations

import os
from typing import Optional

import pytest

from . import recorder
from .ids import TITLE_SEP, compute_id

__all__ = ["step", "compute_id", "TITLE_SEP"]


class step:
    """`with localci_pytest.step("add to cart"):` shows the block as a step of the test on localci's timeline.

    The name of the steps around a failure becomes the failure's `step`. Outside a test, or when the plugin is
    inactive, it does nothing.
    """

    def __init__(self, name: str) -> None:
        self.name = name
        self._sid: Optional[str] = None

    def __enter__(self) -> "step":
        if recorder.active is not None:
            self._sid = recorder.active.step_begin(self.name, "step")
        return self

    def __exit__(self, exc_type: object, exc: Optional[BaseException], tb: object) -> None:
        if self._sid is not None and recorder.active is not None:
            ok = exc is None or isinstance(exc, pytest.skip.Exception)
            recorder.active.step_end(self._sid, ok, None if ok else exc)
        self._sid = None


def pytest_configure(config: pytest.Config) -> None:
    report = os.environ.get("LOCALCI_REPORT")
    if not report:
        return
    rec = recorder.Recorder(config, report)
    config.pluginmanager.register(rec, "localci_pytest_recorder")
    recorder.active = rec


def pytest_unconfigure(config: pytest.Config) -> None:
    rec = config.pluginmanager.get_plugin("localci_pytest_recorder")
    if rec is not None:
        config.pluginmanager.unregister(rec)
        rec.close()
        if recorder.active is rec:
            recorder.active = None
