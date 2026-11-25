"""Test ids: the same function as `packages/playwright/src/ids.ts`, so both packages agree."""
from __future__ import annotations

import hashlib
import json
import os
from typing import List, NamedTuple

import pytest

# Joins class names and the test name the way every event prints them.
TITLE_SEP = " › "

# Nodes that are places tests live in, not part of a test's name.
_CONTAINERS = tuple(
    c for c in (pytest.Session, getattr(pytest, "Directory", None), pytest.Package, pytest.File) if c is not None
)


def compute_id(project: str, file: str, titles: List[str]) -> str:
    """sha1 of `JSON.stringify([project, file, titles])`, first 16 hex characters."""
    payload = json.dumps([project, file, list(titles)], separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha1(payload.encode("utf-8")).hexdigest()[:16]


class Described(NamedTuple):
    id: str
    file: str
    titles: List[str]
    title: str
    line: int


def project_name(config: pytest.Config) -> str:
    return os.environ.get("LOCALCI_PROJECT") or config.rootpath.name


def rel_file(rootpath: object, path: object) -> str:
    """Path relative to the rootdir, forward slashes."""
    try:
        rel = os.path.relpath(str(path), str(rootpath))
    except ValueError:  # another drive on Windows
        rel = str(path)
    return rel.replace(os.sep, "/")


def describe(item: pytest.Item, config: pytest.Config) -> Described:
    file = rel_file(config.rootpath, item.path)
    titles = [n.name for n in item.listchain() if not isinstance(n, _CONTAINERS)]
    try:
        lineno = item.reportinfo()[1]  # 0-based, None when unknown
    except Exception:
        lineno = None
    line = 0 if lineno is None else lineno + 1
    return Described(compute_id(project_name(config), file, titles), file, titles, TITLE_SEP.join(titles), line)
