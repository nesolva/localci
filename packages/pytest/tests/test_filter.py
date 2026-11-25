"""`$LOCALCI_TESTS`: a shard runs the ids it was given and nothing else."""
from conftest import SHOP, assert_valid, by_title, run_pytest, summary, write_project


def ran(run):
    return {title for title in by_title(run.events)}


def test_filter_runs_only_the_listed_ids_and_deselects_the_rest(tmp_path):
    project = write_project(tmp_path / "acme-shop", SHOP)
    everything = by_title(run_pytest(project).events)
    pick = ["TestCheckout › test_pays", "test_prices[2-x y]"]
    ids = tmp_path / "ids.txt"
    ids.write_text("\n".join(everything[t]["test"] for t in pick) + "\nffffffffffffffff\n")  # an unknown id is ignored
    run = run_pytest(project, env={"LOCALCI_TESTS": str(ids)})
    assert run.status == 0
    assert_valid(run.events)
    assert ran(run) == set(pick)
    assert run.events[0]["total"] == 2
    assert summary(run.out) == "2 passed, 7 deselected"  # deselected, not skipped


def test_filter_with_an_empty_file_runs_nothing_and_still_ends_the_report(tmp_path):
    project = write_project(tmp_path / "acme-shop", SHOP)
    ids = tmp_path / "ids.txt"
    ids.write_text("")
    run = run_pytest(project, env={"LOCALCI_TESTS": str(ids)})
    assert [e["t"] for e in run.events] == ["begin", "end"]
    assert (run.events[0]["total"], run.events[1]["status"]) == (0, "passed")


def test_filter_that_cannot_be_read_fails_instead_of_running_everything(tmp_path):
    project = write_project(tmp_path / "acme-shop", SHOP)
    run = run_pytest(project, env={"LOCALCI_TESTS": str(tmp_path / "missing.txt")})
    assert run.status != 0
    assert not [e for e in run.events if e["t"] == "test.begin"]
