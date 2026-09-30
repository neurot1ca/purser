import json
from pathlib import Path
import subprocess
import sys
import uuid

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import maintenance_reports


@pytest.fixture
def project():
    # Keep all test scratch in the repository's ignored scratch directory.
    root = Path(__file__).parents[4] / ".tmp" / "maintenance-reports" / uuid.uuid4().hex
    root.mkdir(parents=True)
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.email", "tests@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Tests"], check=True)
    (root / "module.py").write_text("def unused(): pass\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(root), "add", "module.py"], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", "initial"], check=True)
    try:
        yield root
    finally:
        import shutil
        shutil.rmtree(root, ignore_errors=True)
        shutil.rmtree(root.with_name(root.name + "-state"), ignore_errors=True)


def _state(project):
    return project.with_name(project.name + "-state")


class FakeServer:
    def __init__(self, state, result):
        self.job_dir = state / "codex_exec_mcp" / "jobs"
        self.job_dir.mkdir(parents=True)
        self.result = result
        self.args = None
        self.records = {}

    def _read_job(self, job_id):
        return self.records.get(job_id)

    def _start_job(self, args):
        self.args = args
        job_id = "a" * 64
        job = {
            "jobId": job_id, "state": "RUNNING", "jobPhase": args["jobPhase"],
            "sandbox": args["sandbox"],
        }
        self.records[job_id] = job
        (self.job_dir / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
        return {"structuredContent": {"jobId": job_id, "state": "RUNNING"}}

    def _result_job(self, _args):
        return {"structuredContent": self.result}


def test_run_rejects_project_without_explicit_opt_in(project):
    with pytest.raises(RuntimeError, match="not opted in.*opt-in"):
        maintenance_reports.run_cleanup(project, state=_state(project), server=object())


def test_duplicate_launch_for_same_project_is_rejected(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    key = maintenance_reports.project_root.project_key(project.resolve())
    active = state / "projects" / key / "maintenance" / "cleanup-active.json"
    active.parent.mkdir(parents=True, exist_ok=True)
    active.write_text('{"state":"RUNNING"}', encoding="utf-8")
    with pytest.raises(RuntimeError, match="already running"):
        maintenance_reports.run_cleanup(project, state=state, server=object())


@pytest.mark.parametrize("stale_kind", ("terminal", "unknown", "expired-without-job"))
def test_stale_active_marker_is_replaced(project, stale_kind):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    key = maintenance_reports.project_root.project_key(project.resolve())
    active_path = state / "projects" / key / "maintenance" / "cleanup-active.json"
    active_path.parent.mkdir(parents=True, exist_ok=True)
    server = FakeServer(state, {"state": "SUCCEEDED", "content": "Report."})
    if stale_kind == "expired-without-job":
        active_path.write_text(json.dumps({"started_at": "19700101T000000.000000Z"}), encoding="utf-8")
    else:
        old_job_id = "b" * 64
        active_path.write_text(json.dumps({"job_id": old_job_id}), encoding="utf-8")
        if stale_kind == "terminal":
            server.records[old_job_id] = {"jobId": old_job_id, "state": "SUCCEEDED"}
    report = maintenance_reports.run_cleanup(project, state=state, server=server)
    assert report.exists() and server.args is not None
    assert not active_path.exists()


def test_failed_job_leaves_durable_failure_record_and_no_report(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    server = FakeServer(state, {"state": "FAILED", "errorCode": "CODEX_FAILED", "message": "failed"})
    with pytest.raises(RuntimeError, match="Codex job FAILED"):
        maintenance_reports.run_cleanup(project, state=state, server=server)
    key = maintenance_reports.project_root.project_key(project.resolve())
    reports = state / "projects" / key / "maintenance" / "reports" / "cleanup"
    failure = next((reports / "failures").glob("*.json"))
    record = json.loads(failure.read_text(encoding="utf-8"))
    assert record["state"] == "FAILED" and "Codex job FAILED" in record["error"]
    assert not list(reports.glob("*.md"))


def test_report_is_atomically_persisted_with_source_and_tool_header(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    monkeypatch.setattr(maintenance_reports, "_tool_info", lambda: (
        "vulture: unavailable", "Vulture unavailable; grep-based search only."))
    replace = maintenance_reports.os.replace
    def verify_atomic_replace(source, destination):
        if Path(destination).suffix == ".md":
            assert Path(source).parent == Path(destination).parent
            assert Path(source).exists() and not Path(destination).exists()
        replace(source, destination)
    monkeypatch.setattr(maintenance_reports.os, "replace", verify_atomic_replace)
    server = FakeServer(state, {"state": "SUCCEEDED", "content": "## Candidates\n\nNone."})
    report = maintenance_reports.run_cleanup(project, state=state, server=server)
    content = report.read_text(encoding="utf-8")
    assert report.parent.name == "cleanup" and report.suffix == ".md"
    assert "Source commit:" in content and "Run time (UTC):" in content
    assert "Python:" in content and "Git:" in content and "vulture: unavailable" in content
    assert "grep-based search only" in content and "## Candidates" in content
    assert server.args["jobPhase"] == "operation" and server.args["sandbox"] == "read-only"
    prompt = server.args["prompt"].lower()
    assert all(term in prompt for term in ("search evidence", "path and symbol", "entry points",
                                            "hooks", "dynamic", "install", "grep-based"))
    assert not list(report.parent.glob("*.tmp"))


def test_report_run_leaves_repository_unchanged(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    before = subprocess.run(["git", "-C", str(project), "status", "--porcelain"],
                           capture_output=True, text=True, check=True).stdout
    maintenance_reports.run_cleanup(project, state=state, server=FakeServer(
        state, {"state": "SUCCEEDED", "content": "Report."}))
    after = subprocess.run(["git", "-C", str(project), "status", "--porcelain"],
                          capture_output=True, text=True, check=True).stdout
    assert after == before == ""


def test_report_job_does_not_create_async_followup_record(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    server = FakeServer(state, {"state": "SUCCEEDED", "content": "Report."})
    maintenance_reports.run_cleanup(project, state=state, server=server)
    assert not (state / "async_jobs").exists()
    record = next((state / "codex_exec_mcp" / "jobs").glob("*.json"))
    assert json.loads(record.read_text(encoding="utf-8"))["jobPhase"] == "operation"
