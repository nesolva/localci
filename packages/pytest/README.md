# localci-pytest

Makes a pytest suite report to [localci](https://github.com/nesolva/localci): tests, steps and failure reports an
agent can act on (failing step, expected vs actual, the failing line), sharded like a Playwright suite. It is made for
HTTP API suites: the service under test and its mocked dependencies run as the suite's `stack`, pytest talks to them.

```bash
pip install localci-pytest        # until it is published: pip install "git+https://github.com/nesolva/localci#subdirectory=packages/pytest"
```

```yaml
# the suite's test section in localci's config
test:
  run: python -m pytest -p localci_pytest
  list: python -m localci_pytest list
```

- `python -m localci_pytest list [pytest args]` prints the suite's tests as NDJSON `{test,file,title,line}` and runs
  none. It exits with 2 if a test file does not collect.
- With `LOCALCI_TESTS=<file of test ids>` only those tests run; the others are deselected.
- Without `LOCALCI_REPORT` the plugin does nothing, so the same suite runs outside localci unchanged, with or without
  `-p localci_pytest`.

```python
import localci_pytest

def test_checkout(api):
    with localci_pytest.step("place the order"):       # a step on localci's timeline
        order = api.post("/orders", {"sku": "mug", "qty": 3})
    with localci_pytest.step("check the total"):
        assert order["total"] == 2700                  # write `assert actual == expected`: both show in the report
```

Fixtures of your own show as `setup: <name>` and, for `yield` fixtures, `teardown: <name>` steps.

Keep pytest's rootdir at the repository's root (a `pytest.ini` or `pyproject.toml` there): a test's `file` is relative to
the rootdir, and file filters and `--affected` read it as relative to the repository. `list` runs without the
suite's `env`: read service URLs inside tests or fixtures, not at import time.

Works with `pytest-rerunfailures` (each attempt is reported) and `pytest-xdist`. Requires Python 3.9+ and pytest 7+;
nothing else. What is reported, and how failures and statuses are mapped, is in
[`docs/protocol.md`](../../docs/protocol.md#pytest-plugin). MIT.
