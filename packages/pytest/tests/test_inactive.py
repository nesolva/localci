"""Without `$LOCALCI_REPORT` the plugin is not there: a developer's plain pytest run is unchanged."""
from conftest import SHOP, run_pytest, summary, write_project


def test_inactive_plain_run_writes_nothing_calls_nothing_and_behaves_as_before(tmp_path, hook_server):
    server = hook_server()
    project = write_project(tmp_path / "acme-shop", SHOP)
    before = sorted(p.name for p in project.rglob("*"))
    ids = tmp_path / "ids.txt"
    ids.write_text("ffffffffffffffff\n")
    # everything localci would set except the report: still no plugin, so no filter, no hook, no file
    run = run_pytest(project, report=False, env={"LOCALCI_API": server.url, "LOCALCI_TESTS": str(ids), "LOCALCI_HOOK_TOKEN": "t"})
    without = run_pytest(project, report=False, plugin=False)
    assert (run.status, summary(run.out)) == (without.status, summary(without.out))
    assert summary(run.out) == "2 failed, 4 passed, 1 skipped, 1 xfailed, 1 error"
    assert server.calls == []
    assert not run.report.exists()
    assert sorted(p.name for p in project.rglob("*") if p.name != "__pycache__" and p.suffix != ".pyc") == before
