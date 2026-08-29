from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import ledger
import policy


def git(repo, *arguments):
    return subprocess.run(
        ["git", *arguments],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
        timeout=10,
    )


@pytest.fixture()
def sandbox_dir():
    root = Path(__file__).parent / ".ledger-test-tmp" / uuid.uuid4().hex
    root.mkdir(parents=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


@pytest.fixture()
def repo(sandbox_dir):
    git(sandbox_dir, "init", "-q")
    git(sandbox_dir, "config", "user.email", "tests@example.invalid")
    git(sandbox_dir, "config", "user.name", "Ledger Tests")
    (sandbox_dir / "src").mkdir()
    (sandbox_dir / "src" / "target.py").write_text("value = 1\n", encoding="utf-8")
    (sandbox_dir / "src" / "helper.py").write_text("helper = 1\n", encoding="utf-8")
    (sandbox_dir / "notes.txt").write_text("original\n", encoding="utf-8")
    (sandbox_dir / "state").mkdir()
    (sandbox_dir / "state" / "current.yaml").write_text("status: initial\n", encoding="utf-8")
    git(sandbox_dir, "add", ".")
    git(sandbox_dir, "commit", "-qm", "initial")
    return sandbox_dir


def test_unstaged_staged_and_scoped_ignored_files_are_union_sources(repo):
    exclude = repo / ".git" / "info" / "exclude"
    exclude.write_text(exclude.read_text(encoding="utf-8") + "\n.claude/\n", encoding="utf-8")
    baseline = ledger.capture_baseline(repo, ["src", ".claude/hooks/orchestrator"])

    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "src" / "staged.py").write_text("staged = True\n", encoding="utf-8")
    git(repo, "add", "src/staged.py")
    ignored = repo / ".claude" / "hooks" / "orchestrator" / "new.py"
    ignored.parent.mkdir(parents=True)
    ignored.write_text("ignored = True\n", encoding="utf-8")

    delta = ledger.compute_delta(repo, baseline)

    assert delta["status"] == ledger.OK
    assert set(delta["changed_paths"]) == {
        "src/target.py",
        "src/staged.py",
        ".claude/hooks/orchestrator/new.py",
    }
    assert delta["sources"]["unstaged"]["paths"] == ["src/target.py"]
    assert delta["sources"]["staged"]["paths"] == ["src/staged.py"]
    assert delta["sources"]["scoped_manifest"]["paths"] == [
        ".claude/hooks/orchestrator/new.py"
    ]
    assert delta["changed_file_count"] == 3
    assert delta["changed_line_count"] >= 3


def test_commit_with_clean_worktree_is_detected_from_baseline_head(repo):
    baseline = ledger.capture_baseline(repo, ["src/target.py"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    git(repo, "add", "src/target.py")
    git(repo, "commit", "-qm", "delegated change")

    assert git(repo, "status", "--porcelain").stdout == ""
    delta = ledger.compute_delta(repo, baseline)

    assert delta["status"] == ledger.OK
    assert delta["baseline_head"] != delta["current_head"]
    assert delta["changed_paths"] == ["src/target.py"]
    assert delta["sources"]["committed"]["paths"] == ["src/target.py"]
    assert delta["changed_file_count"] == 1
    assert delta["changed_line_count"] == 2


def test_preexisting_disjoint_dirty_change_is_reported_but_not_reviewed_or_reverted(repo):
    (repo / "notes.txt").write_text("user work\n", encoding="utf-8")
    baseline = ledger.capture_baseline(repo, ["src/target.py"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")

    delta = ledger.compute_delta(repo, baseline)
    classification = ledger.classify_concurrent_writers(delta, ["src/target.py"])

    assert delta["changed_paths"] == ["src/target.py"]
    assert "notes.txt" in delta["preexisting_paths"]
    assert classification["decision"] == ledger.PROCEED
    assert classification["preserved_out_of_scope_paths"] == ["notes.txt"]
    assert classification["review_changed_file_count"] == 1
    assert (repo / "notes.txt").read_text(encoding="utf-8") == "user work\n"


def test_disjoint_out_of_scope_change_proceeds_and_is_excluded_from_review(repo):
    baseline = ledger.capture_baseline(repo, ["src"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "src" / "helper.py").write_text("helper = 2\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)

    classification = ledger.classify_concurrent_writers(delta, ["src/target.py"])

    assert classification["decision"] == ledger.PROCEED
    assert classification["in_scope_paths"] == ["src/target.py"]
    assert classification["out_of_scope_paths"] == ["src/helper.py"]
    assert classification["preserved_out_of_scope_paths"] == ["src/helper.py"]
    assert classification["overlap_paths"] == []
    assert classification["review_changed_file_count"] == 1


def test_ambiguous_same_path_overlap_halts(repo):
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    baseline = ledger.capture_baseline(repo, ["src/target.py"])
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)

    classification = ledger.classify_concurrent_writers(delta, ["src/target.py"])

    assert delta["ambiguous_paths"] == ["src/target.py"]
    assert classification["decision"] == ledger.UNKNOWN_CONCURRENT_WRITER
    assert classification["overlap_paths"] == ["src/target.py"]
    assert classification["halt_paths"] == ["src/target.py"]


def test_out_of_scope_material_boundary_halts(repo):
    baseline = ledger.capture_baseline(repo, ["src"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "src" / "helper.py").write_text("helper = 2\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)

    classification = ledger.classify_concurrent_writers(
        delta, ["src/target.py"], material_paths=["src/helper.py"]
    )

    assert classification["decision"] == ledger.UNKNOWN_CONCURRENT_WRITER
    assert classification["material_boundary_paths"] == ["src/helper.py"]
    assert classification["halt_paths"] == ["src/helper.py"]


def test_out_of_scope_verification_boundary_halts(repo):
    baseline = ledger.capture_baseline(repo, ["src"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "src" / "helper.py").write_text("helper = 2\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)

    classification = ledger.classify_concurrent_writers(
        delta, ["src/target.py"], verification_paths=["src/helper.py"]
    )

    assert classification["decision"] == ledger.UNKNOWN_CONCURRENT_WRITER
    assert classification["verification_boundary_paths"] == ["src/helper.py"]
    assert classification["halt_paths"] == ["src/helper.py"]


def test_protected_path_halts(repo):
    baseline = ledger.capture_baseline(repo, ["src", "state/current.yaml"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "state" / "current.yaml").write_text("status: changed\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)

    classification = ledger.classify_concurrent_writers(delta, ["src/target.py"])

    assert classification["decision"] == ledger.UNKNOWN_CONCURRENT_WRITER
    assert classification["protected_paths"] == ["state/current.yaml"]
    assert classification["halt_paths"] == ["state/current.yaml"]


def test_git_failure_and_timeout_are_unknown_not_empty_delta(repo, sandbox_dir):
    failed = ledger.capture_baseline(sandbox_dir / "not-a-repository", ["src"])
    timed_out = ledger.capture_baseline(repo, ["src"], timeout=0)

    for result in (failed, timed_out):
        assert result["status"] == ledger.UNKNOWN
        assert result["changed_paths"] is None
        assert "changed_file_count" not in result
        assert "changed_line_count" not in result
    assert failed["code"] in {"GIT_FAILED", "GIT_UNAVAILABLE", "REPOSITORY_IO_FAILED"}
    assert timed_out["code"] == "GIT_TIMEOUT"


def test_whole_repository_manifest_scope_is_rejected_before_git_enumeration(repo, monkeypatch):
    monkeypatch.setattr(
        ledger,
        "_run_git",
        lambda *args, **kwargs: pytest.fail("Git must not run for an unbounded scope"),
    )

    result = ledger.capture_baseline(repo, ["."])

    assert result["status"] == ledger.UNKNOWN
    assert result["code"] == "UNBOUNDED_SCOPE"
    assert result["changed_paths"] is None


def test_manifest_enumeration_passes_only_literal_declared_scope_to_git(repo, monkeypatch):
    calls = []
    real_run_git = ledger._run_git

    def recording_run_git(repo_root, arguments, timeout, operation):
        if arguments[:2] == ["ls-files", "--others"]:
            calls.append(arguments)
        return real_run_git(repo_root, arguments, timeout, operation)

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)

    result = ledger.capture_baseline(repo, ["src/target.py"])

    assert result["status"] == ledger.OK
    assert len(calls) == 2
    for arguments in calls:
        separator = arguments.index("--")
        assert arguments[separator + 1 :] == [":(literal)src/target.py"]


def test_manifest_budget_is_not_applied_to_tracked_git_commands(repo, monkeypatch):
    observed = []
    real_run_git = ledger._run_git

    def recording_run_git(repo_root, arguments, timeout, operation):
        observed.append((operation, timeout))
        return real_run_git(repo_root, arguments, timeout, operation)

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)

    result = ledger.capture_baseline(
        repo,
        ["src/target.py"],
        timeout=10.0,
        manifest_budget_seconds=1.0,
    )

    assert result["status"] == ledger.OK
    manifest_operations = {
        "listing scoped untracked files",
        "listing scoped ignored files",
    }
    assert all(
        timeout == 10.0
        for operation, timeout in observed
        if operation not in manifest_operations
    )
    assert all(
        0 < timeout <= 1.0
        for operation, timeout in observed
        if operation in manifest_operations
    )


def test_manifest_budget_exhaustion_is_typed_unknown_for_baseline_and_delta(repo):
    exhausted_baseline = ledger.capture_baseline(
        repo,
        ["src"],
        manifest_budget_seconds=1e-12,
    )
    baseline = ledger.capture_baseline(repo, ["src"])
    exhausted_delta = ledger.compute_delta(
        repo,
        baseline,
        manifest_budget_seconds=1e-12,
    )

    for result in (exhausted_baseline, exhausted_delta):
        assert result["status"] == ledger.UNKNOWN
        assert result["code"] == ledger.MANIFEST_BUDGET_EXCEEDED
        assert result["changed_paths"] is None
        assert "changed_file_count" not in result
        assert "changed_line_count" not in result


def test_unknown_baseline_cannot_become_an_empty_delta(repo):
    result = ledger.compute_delta(
        repo,
        {"status": ledger.UNKNOWN, "code": "GIT_FAILED", "changed_paths": None},
    )

    assert result["status"] == ledger.UNKNOWN
    assert result["code"] == "BASELINE_INVALID"
    assert result["changed_paths"] is None
    assert "changed_file_count" not in result


def test_protected_paths_are_detected_only(repo):
    paths = [
        "state/current.yaml",
        "AGENTS.md",
        "docs/EVIDENCE_RULES.md",
        "inputs/run.json",
        "review_artifacts/report.json",
        ".gitattributes",
        "src/target.py",
    ]

    assert ledger.detect_protected_paths(paths) == sorted(paths[:-1])


def test_session_record_is_checksummed_and_feeds_thread_policy(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        "thread-123",
        "normal",
        "gpt-5.6-sol",
        "medium",
        pending_call_baseline={"status": ledger.OK, "head": "abc"},
        last_outcome={"validation": "PASS"},
    )

    assert store.save(record)["status"] == ledger.OK
    loaded = store.load("task-1")

    assert loaded["status"] == ledger.OK
    assert loaded["record"] == record
    requested = {
        "task_id": "task-1",
        "route": "normal",
        "model": "gpt-5.6-sol",
        "effort": "medium",
    }
    assert policy.thread_compatibility(requested, loaded["record"]["live_thread"]).compatible


def test_pending_baseline_can_be_saved_before_a_fresh_thread_exists(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        None,
        "normal",
        "gpt-5.6-sol",
        "medium",
        pending_call_baseline={"status": ledger.OK, "head": "abc"},
    )

    assert store.save(record)["status"] == ledger.OK
    loaded = store.load("task-1")
    assert loaded["status"] == ledger.OK
    assert loaded["record"]["live_thread"] is None
    requested = {
        "task_id": "task-1",
        "route": "normal",
        "model": "gpt-5.6-sol",
        "effort": "medium",
    }
    assert policy.thread_compatibility(requested, None).code == "NO_LIVE_THREAD"


def test_truncated_or_checksum_corrupt_record_is_not_a_valid_thread(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1", "thread-123", "normal", "gpt-5.6-sol", "medium"
    )
    assert store.save(record)["status"] == ledger.OK
    path = store.path_for_task("task-1")
    raw = path.read_bytes()

    path.write_bytes(raw[: len(raw) // 2])
    truncated = store.load("task-1")
    assert truncated == {
        "status": ledger.CORRUPT,
        "code": "LEDGER_CORRUPT",
        "record": None,
    }

    assert store.save(record)["status"] == ledger.OK
    path.write_bytes(path.read_bytes().replace(b"thread-123", b"thread-999"))
    checksum_corrupt = store.load("task-1")
    assert checksum_corrupt["status"] == ledger.CORRUPT
    assert checksum_corrupt["record"] is None
