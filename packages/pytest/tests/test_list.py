"""`python -m localci_pytest list`: the suite as NDJSON, nothing run."""
from conftest import SHOP, write_project
from test_ids import list_ids


def test_list_prints_one_json_line_per_test_and_runs_no_test(tmp_path):
    files = {
        "test_a.py": "from pathlib import Path\n\ndef test_ran():\n    Path('ran.marker').write_text('x')\n\n"
        "def test_other():\n    pass\n",
    }
    project = write_project(tmp_path / "acme-shop", files)
    r, tests = list_ids(project)
    assert r.returncode == 0
    assert [(t["file"], t["title"], t["line"]) for t in tests] == [("test_a.py", "test_ran", 3), ("test_a.py", "test_other", 6)]
    assert all(set(t) == {"test", "file", "title", "line"} and len(t["test"]) == 16 for t in tests)
    assert not (project / "ran.marker").exists()
    assert all(line.startswith("{") for line in r.stdout.splitlines()), "stdout is the listing and nothing else"


def test_list_passes_pytest_arguments_on(tmp_path):
    project = write_project(tmp_path / "acme-shop", SHOP)
    r, tests = list_ids(project, args=["-k", "Coupons"])
    assert [t["title"] for t in tests] == ["TestCheckout › TestCoupons › test_expired"]


def test_list_fails_when_a_test_file_does_not_collect(tmp_path):
    # exit 0 here would drop that file's tests from the run without a word
    project = write_project(tmp_path / "acme-shop", {"test_ok.py": "def test_a():\n    pass\n", "test_broken.py": "import nothing_here_exists\n"})
    r, _ = list_ids(project)
    assert r.returncode == 2
    assert "nothing_here_exists" in r.stderr
