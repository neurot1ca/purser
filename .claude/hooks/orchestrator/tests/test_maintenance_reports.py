import json
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
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


def _eligible_project(project, state):
    maintenance_reports.opt_in(project, state=state)
    maintenance_reports.record_activity(project, state=state)
    key = maintenance_reports.project_root.project_key(project.resolve())
    directory = state / "projects" / key / "maintenance"
    old = __import__("time").time() - maintenance_reports.QUIET_PERIOD_SECONDS - 1
    (directory / "last-activity.json").write_text(json.dumps({"at": old}), encoding="utf-8")
    (directory / "last-run.json").write_text(json.dumps({
        "completed_at": old - maintenance_reports.MAINTENANCE_CADENCE_SECONDS - 1}), encoding="utf-8")
    return directory


def _report_notice(project, state, *, shown=False):
    directory = (state / "projects" / maintenance_reports.project_root.project_key(project.resolve())
                 / "maintenance" / "reports" / "cleanup")
    directory.mkdir(parents=True, exist_ok=True)
    report = directory / "report.md"
    report.write_text("report", encoding="utf-8")
    notice_path = report.with_suffix(".notice.json")
    notice_path.write_text(json.dumps({"report_path": str(report), "findings": [],
                                       "finding_count": 0, "shown": shown}), encoding="utf-8")
    return notice_path


def _quota(label, used, cycle):
    return {f"{label}_used_pct": used, f"{label}_resets_at": cycle}


def test_global_slot_race_admits_exactly_one_project(project):
    state = _state(project)
    slot = state / "maintenance" / "active.json"
    barrier = threading.Barrier(2)
    winners = []
    def contend():
        barrier.wait()
        try:
            with maintenance_reports._global_slot(state):
                winners.append(True)
                __import__("time").sleep(0.02)
        except FileExistsError:
            winners.append(False)
    threads = [threading.Thread(target=contend) for _ in range(2)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert sorted(winners) == [False, True]
    assert not slot.exists()


@pytest.mark.parametrize("stale_reason", ("dead-pid", "expired"))
def test_global_slot_recovers_dead_or_expired_owner(project, monkeypatch, stale_reason):
    state = _state(project)
    slot = state / "maintenance" / "active.json"
    slot.parent.mkdir(parents=True)
    monkeypatch.setattr(maintenance_reports, "_pid_alive", lambda _pid: stale_reason != "dead-pid")
    started_at = (__import__("time").time() - maintenance_reports.BACKGROUND_CHECK_TIMEOUT_SECONDS - 1
                  if stale_reason == "expired" else __import__("time").time())
    slot.write_text(json.dumps({"pid": 123456, "started_at": started_at}), encoding="utf-8")
    with maintenance_reports._global_slot(state):
        current = json.loads(slot.read_text(encoding="utf-8"))
        assert current["pid"] == os.getpid()
        assert current["started_at"] > started_at
    assert not slot.exists()


@pytest.mark.parametrize(("identity", "alive"), (("DEAD", False), ("ALIVE", True), ("UNCONFIRMED", True)))
def test_windows_pid_liveness_uses_process_identity_without_os_kill(monkeypatch, identity, alive):
    import codex_exec_mcp
    monkeypatch.setattr(maintenance_reports.os, "name", "nt")
    monkeypatch.setattr(codex_exec_mcp, "_windows_process_identity", lambda _pid: (identity, None))
    monkeypatch.setattr(maintenance_reports.os, "kill",
                        lambda *_args: pytest.fail("Windows liveness must not call os.kill"))
    assert maintenance_reports._pid_alive(4242) is alive


def test_idle_check_skips_stale_quota(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: None)
    assert not maintenance_reports.eligible(project, state=state)


def test_idle_check_requires_quiet_period(project, monkeypatch):
    state = _state(project)
    directory = _eligible_project(project, state)
    (directory / "last-activity.json").write_text(json.dumps({"at": __import__("time").time()}), encoding="utf-8")
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "5h_used_pct": 1, "5h_resets_at": "cycle-a"})
    assert not maintenance_reports.eligible(project, state=state)


def test_idle_check_rejects_quota_above_named_threshold(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "5h_used_pct": maintenance_reports.QUOTA_MAX_USED_PERCENT + 1, "5h_resets_at": "cycle-a"})
    assert not maintenance_reports.eligible(project, state=state)


def test_idle_gate_prefers_present_five_hour_window(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        **_quota("5h", 40, "five"), **_quota("7d", 99, "week")})
    assert maintenance_reports.eligible(project, state=state)


def test_idle_gate_uses_weekly_only_window(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("7d", 50, "week"))
    assert maintenance_reports.eligible(project, state=state)


@pytest.mark.parametrize("decision,expected", [("yes", True), ("no", False)])
def test_no_window_requires_and_records_consent(project, monkeypatch, decision, expected):
    state = _state(project)
    directory = _eligible_project(project, state)
    (directory / "consent.json").unlink()
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {"snapshot_at": "fresh"})
    assert not maintenance_reports.eligible(project, state=state)
    maintenance_reports.record_consent(project, decision, state=state)
    assert maintenance_reports.eligible(project, state=state) is expected


def test_no_window_consent_notice_is_shown_once(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    key = maintenance_reports.project_root.project_key(project.resolve())
    (state / "projects" / key / "maintenance" / "consent.json").unlink()
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "snapshot_at": "fresh", "windows_seen": False})
    notice = maintenance_reports.consent_context(project, state=state)
    assert notice["kind"] == "consent"
    assert "billed to your API account" in maintenance_reports.format_notice(notice)
    assert maintenance_reports.consent_context(project, state=state) is None


def test_expired_quota_windows_do_not_trigger_api_consent(project, monkeypatch):
    state = _state(project)
    directory = _eligible_project(project, state)
    (directory / "consent.json").unlink()
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "snapshot_at": "fresh", "windows_seen": True})
    assert maintenance_reports.consent_context(project, state=state) is None


def test_notice_context_skips_quota_read_without_candidate_reports(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota",
                        lambda *_args, **_kwargs: pytest.fail("no candidate report; skip quota read"))
    assert maintenance_reports.notice_context(project, state=state) is None


def test_report_first_show_at_seventy_percent_remaining(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    path = _report_notice(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("5h", 30, "cycle-a"))
    first = maintenance_reports.notice_context(project, state=state)
    assert first["notice_file"] == str(path)
    assert "Skip showing this again in this cycle?" in maintenance_reports.format_notice(first)


def test_report_first_show_is_immediate_when_session_starts_at_sixty_remaining(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    _report_notice(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("5h", 40, "cycle-a"))
    assert maintenance_reports.notice_context(project, state=state) is not None


def test_report_shows_second_time_at_thirty_remaining(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    path = _report_notice(project, state)
    quota = {"value": _quota("5h", 30, "cycle-a")}
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: quota["value"])
    first = maintenance_reports.notice_context(project, state=state)
    assert first["pending_skip_second"] is False
    assert "Skip showing this again in this cycle?" in maintenance_reports.format_notice(first)
    maintenance_reports.confirm_notice(first["notice_file"])
    assert maintenance_reports.notice_context(project, state=state) is None
    quota["value"] = _quota("5h", 70, "cycle-a")
    second = maintenance_reports.notice_context(project, state=state)
    assert second["notice_file"] == str(path)
    assert "Skip showing this again in this cycle?" not in maintenance_reports.format_notice(second)
    maintenance_reports.confirm_notice(second["notice_file"])
    assert maintenance_reports.notice_context(project, state=state) is None


def test_skip_second_show_applies_only_to_current_cycle(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    path = _report_notice(project, state)
    quota = {"value": _quota("5h", 30, "cycle-a")}
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: quota["value"])
    first = maintenance_reports.notice_context(project, state=state)
    assert "Skip showing this again in this cycle?" in maintenance_reports.format_notice(first)
    maintenance_reports.confirm_notice(first["notice_file"])
    maintenance_reports.skip_second_show(project, first["notice_file"], state=state)
    quota["value"] = _quota("5h", 70, "cycle-a")
    assert maintenance_reports.notice_context(project, state=state) is None
    quota["value"] = _quota("5h", 30, "cycle-b")
    assert maintenance_reports.notice_context(project, state=state)["notice_file"] == str(path)


def test_report_start_below_thirty_remaining_shows_once_per_cycle(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    _report_notice(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("7d", 75, "cycle-a"))
    first = maintenance_reports.notice_context(project, state=state)
    assert first["pending_skip_second"] is True
    assert "Skip showing this again in this cycle?" not in maintenance_reports.format_notice(first)
    maintenance_reports.confirm_notice(first["notice_file"])
    assert maintenance_reports.notice_context(project, state=state) is None


def test_no_window_report_has_no_second_show_question(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    _report_notice(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "snapshot_at": "fresh", "windows_seen": False})
    notice = maintenance_reports.notice_context(project, state=state)
    assert notice["pending_cycle"] == "no-window"
    assert "Skip showing this again in this cycle?" not in maintenance_reports.format_notice(notice)


def test_completed_cleanup_marks_report_done_permanently(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    path = _report_notice(project, state)
    maintenance_reports.mark_report_done(project, str(path), state=state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("5h", 0, "cycle-a"))
    assert maintenance_reports.notice_context(project, state=state) is None
    assert json.loads(path.read_text(encoding="utf-8"))["done"] is True


def test_opt_out_stops_automatic_runs(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: _quota("5h", 1, "cycle-a"))
    assert maintenance_reports.eligible(project, state=state)
    maintenance_reports.opt_out(project, state=state)
    assert not maintenance_reports.eligible(project, state=state)


def test_session_start_wake_is_detached_without_starting_a_process(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    calls = []
    monkeypatch.setattr(maintenance_reports, "_canonical_root", lambda value: Path(value).resolve())
    monkeypatch.setattr(maintenance_reports.subprocess, "Popen", lambda *args, **kwargs: calls.append((args, kwargs)))
    assert maintenance_reports.wake_background_check(project, state=state)
    assert calls and calls[0][1]["stdin"] == subprocess.DEVNULL
    assert calls[0][1]["stdout"] == subprocess.DEVNULL and calls[0][1]["stderr"] == subprocess.DEVNULL


def test_idle_check_skips_active_user_job(project, monkeypatch):
    state = _state(project)
    _eligible_project(project, state)
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    (jobs / ("c" * 64 + ".json")).write_text(json.dumps({"state": "RUNNING"}), encoding="utf-8")
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "5h_used_pct": 1, "5h_resets_at": "cycle-a"})
    assert not maintenance_reports.eligible(project, state=state)


def test_idle_check_respects_seven_day_cadence(project, monkeypatch):
    state = _state(project)
    directory = _eligible_project(project, state)
    (directory / "last-run.json").write_text(json.dumps({"completed_at": __import__("time").time()}), encoding="utf-8")
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "5h_used_pct": 1, "5h_resets_at": "cycle-a"})
    assert not maintenance_reports.eligible(project, state=state)


def test_test_gaps_uses_opt_in_slot_and_its_own_cadence_and_report_dir(project, monkeypatch):
    state = _state(project)
    directory = _eligible_project(project, state)
    cleanup_last_run = json.loads((directory / "last-run.json").read_text(encoding="utf-8"))
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {
        "5h_used_pct": 1, "5h_resets_at": "cycle-a"})
    assert maintenance_reports.eligible(project, state=state, report_type="test-gaps")
    server = FakeServer(state, {"state": "SUCCEEDED", "content": """
```purser-findings-json
{"findings":[{"path_line":"module.py:1","symbol":"unused","evidence":"no test exercises this behavior","confidence":"high"}],"line_coverage_pct":91.2}
```
"""})
    report = maintenance_reports.run_test_gaps(project, state=state, server=server)
    assert report.parent.name == "test-gaps" and report.exists()
    assert "read-only" in server.args["prompt"] and "line_coverage_pct" in server.args["prompt"]
    assert not (state / "maintenance" / "active.json").exists()
    notice = json.loads(report.with_suffix(".notice.json").read_text(encoding="utf-8"))
    assert notice["line_coverage_pct"] == 91.2
    assert (report.parent / "last-run.json").exists()
    assert not maintenance_reports.eligible(project, state=state, report_type="test-gaps")
    assert json.loads((directory / "last-run.json").read_text(encoding="utf-8")) == cleanup_last_run


def test_due_report_type_uses_oldest_last_run_and_cleanup_wins_ties(tmp_path):
    cleanup = tmp_path / "last-run.json"
    gaps = tmp_path / "reports" / "test-gaps" / "last-run.json"
    cleanup.parent.mkdir(parents=True, exist_ok=True)
    gaps.parent.mkdir(parents=True)
    cleanup.write_text('{"completed_at":20}', encoding="utf-8")
    gaps.write_text('{"completed_at":10}', encoding="utf-8")
    assert maintenance_reports._select_due_report_type(tmp_path) == "test-gaps"
    gaps.write_text('{"completed_at":20}', encoding="utf-8")
    assert maintenance_reports._select_due_report_type(tmp_path) == "cleanup"


def test_test_gaps_notice_and_coverage_milestone():
    notice = {"report_type": "test-gaps", "report_path": "C:/state/report.md",
              "finding_count": 1, "line_coverage_pct": 91,
              "findings": [{"path_line": "module.py:8", "symbol": "parse",
                            "evidence": "branch lacks test", "confidence": "high"}]}
    text = maintenance_reports.format_notice(notice)
    assert "lists 1 missing tests" in text
    assert "Gap | Evidence" in text and "module.py:8 parse" in text
    assert "report is at `C:/state/report.md`" in text
    assert "91% covered by tests" in text
    assert "clean up" not in text.casefold()
    for coverage in (90, None):
        notice["line_coverage_pct"] = coverage
        assert "covered by tests" not in maintenance_reports.format_notice(notice)


def test_notice_formats_all_findings_up_to_five_and_claims_once(project, monkeypatch):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    reports = state / "projects" / maintenance_reports.project_root.project_key(project.resolve()) / "maintenance" / "reports" / "cleanup"
    reports.mkdir(parents=True)
    path = reports / "one.md"
    path.write_text("report", encoding="utf-8")
    findings = [{"path_line": f"m{i}.py:{i+1}", "symbol": f"dead{i}", "evidence": "no references found",
                 "confidence": "high", "confidence_rank": 3} for i in range(5)]
    notice_path = path.with_suffix(".notice.json")
    notice_path.write_text(json.dumps({"report_path": str(path), "findings": findings,
                                       "finding_count": 5, "shown": False}), encoding="utf-8")
    monkeypatch.setattr(maintenance_reports, "_fresh_quota", lambda _now=None: {"snapshot_at": "fresh"})
    notice = maintenance_reports.notice_context(project, state=state)
    text = maintenance_reports.format_notice(notice)
    assert text.count("| m") == 5 and "the report has 5 findings" in text
    assert maintenance_reports.notice_context(project, state=state) is None
    maintenance_reports.confirm_notice(notice["notice_file"])
    assert json.loads(notice_path.read_text(encoding="utf-8"))["shown"] is True


def test_notice_shows_top_five_by_confidence_and_report_path(project):
    findings = [{"path_line": f"m{i}.py:{i+1}", "symbol": f"dead{i}", "evidence": f"evidence {i}",
                 "confidence": level, "confidence_rank": rank}
                for i, (level, rank) in enumerate([("low", 1), ("high", 3), ("medium", 2),
                                                    ("low", 1), ("high", 3), ("medium", 2)])]
    text = maintenance_reports.format_notice({"report_path": "C:/state/report.md", "findings": findings,
                                               "finding_count": 6})
    assert text.count("| m") == 5 and "m1.py:2" in text and "m3.py:4" not in text
    assert "C:/state/report.md" in text


def test_findings_parse_failure_keeps_report_count_unknown_and_path(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    report = maintenance_reports.run_cleanup(project, state=state, server=FakeServer(
        state, {"state": "SUCCEEDED", "content": "Some prose without a findings block."}))
    content = report.read_text(encoding="utf-8")
    notice = json.loads(report.with_suffix(".notice.json").read_text(encoding="utf-8"))
    rendered = maintenance_reports.format_notice(notice)
    assert "Findings: unknown" in content and notice["finding_count"] is None
    assert "| --- | --- |" not in rendered and str(report) in rendered


def test_user_job_starting_during_maintenance_is_not_cancelled_or_waited_on(project):
    state = _state(project)
    maintenance_reports.opt_in(project, state=state)
    server = FakeServer(state, {"state": "RUNNING"})
    user_job = {"jobId": "d" * 64, "state": "RUNNING", "jobPhase": "implementation"}
    original_start = server._start_job
    cancelled = []
    def start(args):
        value = original_start(args)
        path = server.job_dir / (user_job["jobId"] + ".json")
        path.write_text(json.dumps(user_job), encoding="utf-8")
        return value
    server._start_job = start
    server._cancel_job = lambda args: cancelled.append(args["jobId"]) or {"structuredContent": {"state": "CANCELLED"}}
    with pytest.raises(RuntimeError, match="yielded to a user"):
        maintenance_reports.run_cleanup(project, state=state, server=server)
    assert cancelled == ["a" * 64]
    assert json.loads((server.job_dir / (user_job["jobId"] + ".json")).read_text(encoding="utf-8"))["state"] == "RUNNING"
