from pathlib import Path
import hashlib
import json
import os
import shutil
import subprocess
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import ledger
import policy


def test_expired_queued_job_releases_scope_before_new_acquire(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    monkeypatch.setattr(store, "_job_state", lambda job_id:
                        "JOB_EXPIRED" if job_id == "b" * 64 else "RUNNING")
    assert store.acquire("blocker", "blocker-task", ["src"], job_id="a" * 64)["status"] == ledger.OK
    assert store.acquire("waiter", "waiter-task", ["src"], job_id="b" * 64,
                         queue_conflict=True)["code"] == "LEASE_QUEUED"
    assert store.owner_state("waiter") == "RELEASED"
    assert store.release("blocker")["status"] == ledger.OK
    assert store.acquire("new", "new-task", ["src"])["status"] == ledger.OK


def test_orphaned_lease_releases_and_promotes_queued_job(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    waiter_call = "queued job"
    waiter_job_id = hashlib.sha256(waiter_call.encode()).hexdigest()
    store.jobs_root.mkdir(parents=True)
    (store.jobs_root / f"{waiter_job_id}.json").write_text(
        json.dumps({"jobId": waiter_job_id, "call": waiter_call, "state": "RUNNING"}),
        encoding="utf-8",
    )

    assert store.acquire("orphan", "orphan-task", ["src"], job_id="a" * 64)["status"] == ledger.OK
    assert store.acquire("waiter", "waiter-task", ["src"], job_id=waiter_job_id,
                         queue_conflict=True)["code"] == "LEASE_QUEUED"
    now[0] += 10 * 60 + 1

    assert store.owner_state("orphan") == "RELEASED"
    assert store.owner_state("waiter") == "ACTIVE"
    orphan = store._read()["owners"]["orphan"]
    assert orphan["released_at"] == now[0]


def test_recent_orphaned_lease_is_kept(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    assert store.acquire("orphan", "orphan-task", ["src"], job_id="a" * 64)["status"] == ledger.OK

    now[0] += 10 * 60 - 1

    assert store.owner_state("orphan") == "ACTIVE"


def test_old_lease_with_existing_job_file_is_kept(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    call = "existing job"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    store.jobs_root.mkdir(parents=True)
    (store.jobs_root / f"{job_id}.json").write_text(
        json.dumps({"jobId": job_id, "call": call, "state": "RUNNING"}), encoding="utf-8"
    )
    assert store.acquire("owner", "owner-task", ["src"], job_id=job_id)["status"] == ledger.OK

    now[0] += 10 * 60 + 1

    assert store.owner_state("owner") == "ACTIVE"


def test_protective_job_lease_expires_at_bounded_retention(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    monkeypatch.setattr(ledger, "JOB_RETENTION_SECONDS", 10)
    monkeypatch.setattr(store, "_job_state", lambda _job_id: "SUCCEEDED")
    assert store.acquire("protective", "task", ["src"], job_id="a" * 64,
                         timeout_seconds=1)["status"] == ledger.OK
    now[0] += 12
    assert store.owner_state("protective") == "RELEASED"


def test_finished_worktree_lease_expires_two_hours_after_finish(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    monkeypatch.setattr(store, "_job_state", lambda _job_id: "SUCCEEDED")
    store.acquire("finished", "task", ["src"], job_id="a" * 64,
                  timeout_seconds=1)

    store.set_retention_after("finished", 2_000.0 + ledger.UNAPPLIED_WORKTREE_LEASE_SECONDS)

    now[0] = 2_000.0 + ledger.UNAPPLIED_WORKTREE_LEASE_SECONDS - 1
    assert store.owner_state("finished") == "ACTIVE"
    now[0] += 1
    assert store.owner_state("finished") == "RELEASED"


def test_default_async_lease_retention_matches_24_hour_job_cap(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    assert store.acquire("default", "task", ["src"], job_id="b" * 64)["status"] == ledger.OK
    owner = store._read()["owners"]["default"]
    assert ledger.ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS == 24 * 60 * 60
    assert owner["retention_after"] == now[0] + 24 * 60 * 60 + ledger.JOB_RETENTION_SECONDS


def test_hot_conflict_selectively_queues_and_expires(tmp_path, monkeypatch):
    store = ledger.ScopeLeases(tmp_path / "projects" / "sample")
    now = [1_000.0]
    monkeypatch.setattr(ledger.time, "time", lambda: now[0])
    store.record_conflicts(["src/hot.py"])
    assert store.acquire("owner", "owner-task", ["src"], job_id="a" * 64)["status"] == ledger.OK
    assert store.acquire("hot", "hot-task", ["src/hot.py"], job_id="b" * 64,
                         allow_overlap=True, hot_path_queue=True)["code"] == "LEASE_QUEUED"
    assert store.acquire("disjoint", "disjoint-task", ["docs"], job_id="c" * 64,
                         allow_overlap=True, hot_path_queue=True)["code"] == "LEASE_ACQUIRED"
    monkeypatch.setattr(store, "_job_state", lambda _job_id: "RUNNING")
    now[0] += ledger.CONFLICT_PATH_RETENTION_SECONDS + 1
    assert store.owner_state("hot") == "ACTIVE"
    assert store._read()["owners"]["hot"]["acquired_at"] == now[0]
    assert store.acquire("expired", "expired-task", ["src/hot.py"], job_id="d" * 64,
                         allow_overlap=True, hot_path_queue=True)["code"] == "LEASE_ACQUIRED"
    assert store._read()["conflicts"] == []


@pytest.fixture()
def tmp_path():
    root = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "ledger-pytest"
        / uuid.uuid4().hex
    )
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


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
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "ledger" / uuid.uuid4().hex
    root.mkdir(parents=True, exist_ok=True)
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


def test_attribution_accepts_sequential_leases_for_one_foreign_task(repo):
    baseline = ledger.capture_baseline(repo, ["notes.txt"])
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    delta = ledger.compute_delta(repo, baseline)
    owners = [{"task_id": "repair", "scope": ["src/target.py"]},
              {"task_id": "repair", "scope": ["src/target.py"]}]

    attributed, known = ledger.attribute_known_concurrent_scopes(delta, ["notes.txt"], owners)

    assert known == [{"path": "src/target.py", "task_id": "repair"}]
    assert attributed["changed_paths"] == []


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

    classification = ledger.classify_concurrent_writers(
        delta,
        ["src/target.py"],
        protected_paths=("state/current.yaml",),
    )

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


def test_index_entries_batches_and_matches_single_call_result(repo, monkeypatch):
    paths = [f"src/generated/{index:04d}-{'x' * 40}.py" for index in range(240)]
    records = {
        path: [
            "100644 2222222222222222222222222222222222222222 0",
            "100644 1111111111111111111111111111111111111111 0",
        ]
        for path in paths
    }

    def recording_run_git(calls):
        def run_git(repo_root, arguments, timeout, operation):
            calls.append(list(arguments))
            pathspecs = set(arguments[arguments.index("--") + 1 :])
            output = bytearray()
            for path, entries in records.items():
                if f":(literal){path}" in pathspecs:
                    for entry in entries:
                        output.extend(f"{entry}\t{path}\0".encode("ascii"))
            return bytes(output)

        return run_git

    single_calls = []
    monkeypatch.setattr(ledger, "_run_git", recording_run_git(single_calls))
    monkeypatch.setattr(ledger, "_INDEX_PATHSPEC_BYTE_BUDGET", 1_000_000)
    single_call_result = ledger._index_entries(repo, paths, 10.0)

    batched_calls = []
    monkeypatch.setattr(ledger, "_run_git", recording_run_git(batched_calls))
    monkeypatch.setattr(
        ledger,
        "_INDEX_PATHSPEC_BYTE_BUDGET",
        8000,
    )
    batched_result = ledger._index_entries(repo, paths, 10.0)

    assert len(single_calls) == 1
    assert len(batched_calls) > 1
    assert batched_result == single_call_result


def test_index_entries_limits_each_invocation_by_encoded_argument_bytes(repo, monkeypatch):
    paths = [f"generated/{index:03d}-{'x' * (index % 17)}.py" for index in range(500)]
    calls = []

    def recording_run_git(repo_root, arguments, timeout, operation):
        calls.append(list(arguments))
        return b""

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)

    assert ledger._index_entries(repo, paths, 10.0) == {path: [] for path in paths}
    assert len(calls) > 1
    for arguments in calls:
        pathspecs = arguments[arguments.index("--") + 1 :]
        assert sum(len(os.fsencode(pathspec)) + 1 for pathspec in pathspecs) <= 8000


def test_index_entries_queries_a_path_longer_than_the_budget(repo, monkeypatch):
    path = "generated/" + "x" * (ledger._INDEX_PATHSPEC_BYTE_BUDGET + 100)
    calls = []

    def recording_run_git(repo_root, arguments, timeout, operation):
        calls.append(list(arguments))
        return b""

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)

    assert ledger._index_entries(repo, [path], 10.0) == {path: []}
    assert len(calls) == 1
    assert f":(literal){path}" in calls[0][calls[0].index("--") + 1 :]


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

    assert ledger.detect_protected_paths(
        paths,
        protected_paths=(
            "state/current.yaml",
            "AGENTS.md",
            "docs/EVIDENCE_RULES.md",
            "inputs/**",
            "review_artifacts/**",
            ".gitattributes",
        ),
    ) == sorted(paths[:-1])


def test_session_record_is_checksummed_and_feeds_thread_policy(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        "thread-123",
        "normal",
        "gpt-6-sol",
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
        "model": "gpt-6-sol",
        "effort": "medium",
    }
    assert policy.thread_compatibility(requested, loaded["record"]["live_thread"]).compatible


def test_session_record_checksum_preserves_whole_number_float_spelling(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-float", None, "normal", "gpt-6-sol", "medium",
        last_outcome={"codex_quota": {"remaining": 27.0, "limit": 60.0}},
    )
    assert store.save(record)["status"] == ledger.OK
    path = store.path_for_task("task-float")
    assert b'"remaining":27.0' in path.read_bytes()
    loaded = store.load("task-float")
    assert loaded["status"] == ledger.OK
    assert loaded["record"]["last_outcome"]["codex_quota"] == {
        "remaining": 27.0, "limit": 60.0,
    }


def test_pending_baseline_can_be_saved_before_a_fresh_thread_exists(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        None,
        "normal",
        "gpt-6-sol",
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
        "model": "gpt-6-sol",
        "effort": "medium",
    }
    assert policy.thread_compatibility(requested, None).code == "NO_LIVE_THREAD"


@pytest.mark.parametrize(
    "path",
    ["scripts/orchestration_token_audit_0.py", "src/wallet_sync.py"],
)
def test_session_record_accepts_repository_paths_with_secret_key_terms(sandbox_dir, path):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        None,
        "normal",
        "gpt-6-sol",
        "medium",
        phase="implementation",
        scope=[path],
        pending_call_baseline={
            "tracked_snapshots": {
                path: {"status": [" M"], "index": [], "worktree": {"kind": "file"}}
            }
        },
    )

    assert store.save(record) == {"status": ledger.OK, "code": "LEDGER_WRITTEN"}


@pytest.mark.parametrize(
    "secret_value",
    ["-----BEGIN PRIVATE KEY-----", "sk-" + "a" * 20],
)
def test_session_record_rejects_secret_values_inside_path_keyed_map(
    sandbox_dir, secret_value
):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        None,
        "normal",
        "gpt-6-sol",
        "medium",
        pending_call_baseline={
            "tracked_snapshots": {
                "src/wallet_sync.py": {"worktree": secret_value}
            }
        },
    )

    assert store.save(record)["code"] == "LEDGER_RECORD_INVALID"


def test_session_record_rejects_secret_named_field(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1",
        None,
        "normal",
        "gpt-6-sol",
        "medium",
        last_outcome={"api_key": "not-a-pattern-matched-value"},
    )

    assert store.save(record)["code"] == "LEDGER_RECORD_INVALID"


def test_session_record_last_outcome_path_key_with_token_round_trips(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-path-key-token",
        None,
        "normal",
        "gpt-6-sol",
        "medium",
        last_outcome={"scripts/codex_token_report.py": 221},
    )

    assert store.save(record)["status"] == ledger.OK
    loaded = store.load("task-path-key-token")
    assert loaded["status"] == ledger.OK
    assert loaded["record"]["last_outcome"]["scripts/codex_token_report.py"] == 221


def test_session_record_last_outcome_plain_api_key_is_rejected(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-plain-api-key",
        None,
        "normal",
        "gpt-6-sol",
        "medium",
        last_outcome={"api_key": "not-a-pattern-matched-value"},
    )

    assert store.save(record)["code"] == "LEDGER_RECORD_INVALID"


def test_session_record_accepts_token_report_snapshot_key_but_rejects_api_key(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    path = "scripts/codex_token_report.py"
    baseline = {"status": ledger.OK, "head": "abc", "scope": [path],
                "manifest_enabled": True, "status_entries": [],
                "tracked_snapshots": {path: {"status": [" M"], "index": [],
                                             "worktree": {"kind": "file"}}},
                "scoped_manifest": []}
    record = ledger.build_session_record(
        "task-1", "thread-123", "normal", "gpt-6-sol", "medium",
        phase="implementation", scope=[path],
        origin_baseline=baseline, origin_scope=[path], post_state=baseline,
        last_outcome={"implementer_report": "Set tool_output_token_limit to 8000."},
    )
    assert store.save(record)["status"] == ledger.OK
    record["last_outcome"]["implementer_report"] = "Observed sk-" + "a" * 20
    rejected = store.save(record)
    assert rejected["code"] == "LEDGER_RECORD_INVALID"
    assert "last_outcome.implementer_report" in rejected["message"]


def test_truncated_or_checksum_corrupt_record_is_not_a_valid_thread(sandbox_dir):
    store = ledger.SessionLedger(sandbox_dir / "ledger")
    record = ledger.build_session_record(
        "task-1", "thread-123", "normal", "gpt-6-sol", "medium"
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


def test_detect_out_of_scope_modifications_catches_untracked_and_modified_files(repo):
    scope = ["src/target.py"]
    baseline = ledger.capture_baseline(repo, scope)
    assert baseline["status"] == ledger.OK

    # 1. Modifying in-scope file produces no out-of-scope escapes
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    res = ledger.detect_out_of_scope_modifications(repo, baseline, scope)
    assert res["status"] == ledger.OK
    assert res["paths"] == []

    # 2. Creating untracked file outside scope is detected
    (repo / "untracked_escape.py").write_text("escape = True\n", encoding="utf-8")
    res = ledger.detect_out_of_scope_modifications(repo, baseline, scope)
    assert res["status"] == ledger.OK
    assert res["paths"] == ["untracked_escape.py"]

    # 3. Modifying existing tracked file outside scope is detected
    (repo / "src" / "helper.py").write_text("helper = 99\n", encoding="utf-8")
    res = ledger.detect_out_of_scope_modifications(repo, baseline, scope)
    assert res["status"] == ledger.OK
    assert res["paths"] == ["src/helper.py", "untracked_escape.py"]


def test_detect_out_of_scope_modifications_fails_closed_on_error(repo, monkeypatch):
    scope = ["src/target.py"]
    baseline = ledger.capture_baseline(repo, scope)
    assert baseline["status"] == ledger.OK

    # Invalid baseline fails closed with UNKNOWN
    bad_res = ledger.detect_out_of_scope_modifications(repo, {"status": "INVALID"}, scope)
    assert bad_res["status"] == ledger.UNKNOWN
    assert bad_res["code"] == "BASELINE_INVALID"

    # Git error fails closed with UNKNOWN
    def fail_git(*args, **kwargs):
        raise ledger._InspectionError("GIT_TIMEOUT", "git timed out")

    monkeypatch.setattr(ledger, "_run_git", fail_git)
    error_res = ledger.detect_out_of_scope_modifications(repo, baseline, scope)
    assert error_res["status"] == ledger.UNKNOWN
    assert error_res["code"] == "GIT_TIMEOUT"


def test_scoped_manifest_and_ambiguity_ignore_transient_bytecode_cache(repo):
    scope = ["tests"]
    (repo / "tests").mkdir(exist_ok=True)
    (repo / "tests" / "test_sample.py").write_text("def test_ok(): pass\n", encoding="utf-8")
    git(repo, "add", "tests/test_sample.py")
    git(repo, "commit", "-qm", "add test")

    pycache = repo / "tests" / "__pycache__"
    pycache.mkdir(exist_ok=True)
    (pycache / "test_sample.cpython-312.pyc").write_bytes(b"\x9d\x00\x00\x00bytecode")

    baseline = ledger.capture_baseline(repo, scope)
    assert baseline["status"] == ledger.OK
    manifest_paths = [entry["path"] for entry in baseline["scoped_manifest"]]
    assert not any("test_sample.cpython-312.pyc" in p for p in manifest_paths)

    (pycache / "test_sample.cpython-312.pyc").write_bytes(b"\x9d\x00\x00\x01mutated_bytecode")
    (repo / "tests" / "test_sample.py").write_text("def test_ok(): assert True\n", encoding="utf-8")

    delta = ledger.compute_delta(repo, baseline, scope)
    assert delta["status"] == ledger.OK
    assert delta["ambiguous_paths"] == []
    assert "tests/test_sample.py" in delta["changed_paths"]
    assert not any("__pycache__" in p for p in delta["changed_paths"])


def test_tracked_bytecode_modification_retains_unknown_concurrent_writer(repo):
    scope = ["src"]
    (repo / "src").mkdir(exist_ok=True)
    (repo / "src" / "main.py").write_text("v = 0\n", encoding="utf-8")
    (repo / "src" / "runtime.pyc").write_bytes(b"\x9d\x00\x00\x00committed")
    git(repo, "add", ".")
    git(repo, "commit", "-qm", "add tracked bytecode")

    (repo / "src" / "runtime.pyc").write_bytes(b"\x9d\x00\x00\x01preexisting_edit")

    baseline = ledger.capture_baseline(repo, scope)
    assert baseline["status"] == ledger.OK

    (repo / "src" / "main.py").write_text("v = 1\n", encoding="utf-8")
    (repo / "src" / "runtime.pyc").write_bytes(b"\x9d\x00\x00\x02later_edit")

    delta = ledger.compute_delta(repo, baseline, scope)
    assert delta["status"] == ledger.OK
    assert "src/runtime.pyc" in delta["ambiguous_paths"]

    classification = ledger.classify_concurrent_writers(delta, scope)
    assert classification["decision"] == ledger.UNKNOWN_CONCURRENT_WRITER


def test_session_record_rejects_grounded_contract_with_missing_objective_or_criteria(tmp_path):
    ledger_instance = ledger.SessionLedger(tmp_path)

    # Invalid: grounded=True but objective is empty
    bad_rec1 = ledger.build_session_record(
        "t-inv1", "th-1", "normal", "gpt-6-sol", "medium",
        phase="implementation", scope=["a.py"],
        task_objective="", acceptance_criteria="valid criteria", task_contract_grounded=True
    )
    res1 = ledger_instance.save(bad_rec1)
    assert res1["status"] == ledger.UNKNOWN
    assert res1["code"] == "LEDGER_RECORD_INVALID"

    # Invalid: grounded=True but criteria is None
    bad_rec2 = ledger.build_session_record(
        "t-inv2", "th-2", "normal", "gpt-6-sol", "medium",
        phase="implementation", scope=["a.py"],
        task_objective="valid objective", acceptance_criteria=None, task_contract_grounded=True
    )
    res2 = ledger_instance.save(bad_rec2)
    assert res2["status"] == ledger.UNKNOWN
    assert res2["code"] == "LEDGER_RECORD_INVALID"

    # Valid: grounded=True with non-empty objective and criteria
    good_rec = ledger.build_session_record(
        "t-inv3", "th-3", "normal", "gpt-6-sol", "medium",
        phase="implementation", scope=["a.py"],
        task_objective="valid objective", acceptance_criteria="valid criteria", task_contract_grounded=True
    )
    res3 = ledger_instance.save(good_rec)
    assert res3["status"] == ledger.OK
    assert res3["code"] == "LEDGER_WRITTEN"


def test_plan_session_record_accepts_empty_scope_without_relaxing_phase_validation(tmp_path):
    store = ledger.SessionLedger(tmp_path)
    plan = ledger.build_session_record(
        "plan-task", "plan-thread", "plan_standard", "gpt-6-sol", "high",
        phase="plan", scope=[],
    )
    assert store.save(plan)["status"] == ledger.OK
    assert store.load("plan-task")["record"] == plan

    invalid_phase = ledger.build_session_record(
        "bad-phase", "thread", "normal", "gpt-6-sol", "high",
        phase="unknown", scope=[],
    )
    assert store.save(invalid_phase)["code"] == "LEDGER_RECORD_INVALID"

    invalid_implementation = ledger.build_session_record(
        "bad-scope", "thread", "normal", "gpt-6-sol", "high",
        phase="implementation", scope=[],
    )
    assert store.save(invalid_implementation)["code"] == "LEDGER_RECORD_INVALID"


def test_job_worktree_reproduces_mixed_staged_unstaged_and_untracked_state(repo, tmp_path):
    (repo / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    git(repo, "add", "src/target.py")
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")
    git(repo, "rm", "src/helper.py")
    binary = b"\x00\x01binary\xff"
    (repo / "untracked.bin").write_bytes(binary)
    executable = repo / "run.sh"
    executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    executable.chmod(0o755)
    git(repo, "add", "run.sh")
    source_mode = git(repo, "ls-files", "--stage", "run.sh").stdout.split()[0]

    result = ledger.create_job_worktree(repo, "a" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    assert result["head"] == git(repo, "rev-parse", "HEAD").stdout.strip()
    assert (worktree / "src" / "target.py").read_text(encoding="utf-8") == "value = 3\n"
    assert not (worktree / "src" / "helper.py").exists()
    assert (worktree / "untracked.bin").read_bytes() == binary
    assert git(worktree, "ls-files", "--stage", "run.sh").stdout.split()[0] == source_mode
    assert git(worktree, "write-tree").stdout.strip() == result["snapshot"]


def _worktree_profile(repo, *, copy=(), setup=None, timeout=600):
    profile = repo / ".orchestrator" / "profile.toml"
    profile.parent.mkdir(parents=True, exist_ok=True)
    lines = ["schema_version = 1", "", "[worktree]"]
    if copy:
        lines.append("copy = [" + ", ".join(json.dumps(path) for path in copy) + "]")
    if setup is not None:
        lines.append("setup = " + json.dumps(setup))
    if timeout != 600:
        lines.append(f"setup_timeout_seconds = {timeout}")
    profile.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _trust_worktree_profile(repo, tmp_path, monkeypatch):
    user_config = tmp_path / "user-profile.toml"
    user_config.write_text(
        "schema_version = 1\n[trust]\npaths = ["
        + json.dumps(str(repo.resolve()))
        + "]\n",
        encoding="utf-8",
    )
    load_profile = ledger.project_profile.load_profile
    monkeypatch.setattr(
        ledger.project_profile,
        "load_profile",
        lambda root: load_profile(root, user_config_path=user_config),
    )


def test_job_worktree_copies_profile_file_and_directory(repo, tmp_path, monkeypatch):
    exclude = repo / ".git" / "info" / "exclude"
    exclude.write_text(exclude.read_text(encoding="utf-8") + "\n.env\nconfig/\n", encoding="utf-8")
    (repo / ".env").write_text("TOKEN=local\n", encoding="utf-8")
    (repo / "config").mkdir()
    (repo / "config" / "local.json").write_text('{"local": true}\n', encoding="utf-8")
    _worktree_profile(repo, copy=(".env", "config", "missing-local-file"))
    _trust_worktree_profile(repo, tmp_path, monkeypatch)

    result = ledger.create_job_worktree(repo, "e" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    assert (worktree / ".env").read_text(encoding="utf-8") == "TOKEN=local\n"
    assert (worktree / "config" / "local.json").read_text(encoding="utf-8") == '{"local": true}\n'


@pytest.mark.parametrize("entry", ["../outside", "/outside", "C:\\outside"])
def test_job_worktree_rejects_unsafe_profile_copy_path(repo, tmp_path, monkeypatch, entry):
    _worktree_profile(repo, copy=(entry,))
    _trust_worktree_profile(repo, tmp_path, monkeypatch)
    with pytest.raises(ValueError, match="worktree copy path"):
        ledger.create_job_worktree(repo, "f" * 64, tmp_path / "worktrees")


def test_job_worktree_runs_profile_setup_in_worktree(repo, tmp_path, monkeypatch):
    command = f'"{sys.executable}" -c "from pathlib import Path; Path(\'setup-cwd.txt\').write_text(str(Path.cwd()))"'
    _worktree_profile(repo, setup=command)
    _trust_worktree_profile(repo, tmp_path, monkeypatch)

    result = ledger.create_job_worktree(repo, "1" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    assert (worktree / "setup-cwd.txt").read_text(encoding="utf-8") == str(worktree)


def test_job_worktree_setup_failure_keeps_worktree_and_reports_output(repo, tmp_path, monkeypatch):
    command = f'"{sys.executable}" -c "import sys; print(\'setup failed marker\'); sys.exit(7)"'
    _worktree_profile(repo, setup=command)
    _trust_worktree_profile(repo, tmp_path, monkeypatch)
    parent = tmp_path / "worktrees"

    with pytest.raises(ledger.WorktreeSetupError) as raised:
        ledger.create_job_worktree(repo, "2" * 64, parent)
    worktree = parent / ("2" * 12)
    assert worktree.is_dir()
    assert raised.value.exit_code == 7
    assert "setup failed marker" in raised.value.output


def test_job_worktree_without_profile_section_keeps_existing_behavior(repo, tmp_path):
    exclude = repo / ".git" / "info" / "exclude"
    exclude.write_text(exclude.read_text(encoding="utf-8") + "\n.env\n", encoding="utf-8")
    (repo / ".env").write_text("TOKEN=local\n", encoding="utf-8")
    result = ledger.create_job_worktree(repo, "3" * 64, tmp_path / "worktrees")
    assert not (Path(result["path"]) / ".env").exists()


def test_job_patch_excludes_preexisting_user_changes_and_applies_to_dirty_tree(repo, tmp_path):
    (repo / "notes.txt").write_text("user change\n", encoding="utf-8")
    result = ledger.create_job_worktree(repo, "b" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 9\n", encoding="utf-8")
    (worktree / "job-new.txt").write_text("job addition\n", encoding="utf-8")

    patch = ledger.job_patch(worktree, result["snapshot"])
    assert "src/target.py" in patch
    assert "job-new.txt" in patch
    assert "notes.txt" not in patch
    before_index = git(repo, "diff", "--cached", "--binary").stdout
    applied = ledger.apply_job_patch(repo, patch)
    assert applied == {"applied": True, "conflicts": []}
    assert (repo / "notes.txt").read_text(encoding="utf-8") == "user change\n"
    assert (repo / "src" / "target.py").read_text(encoding="utf-8") == "value = 9\n"
    assert (repo / "job-new.txt").read_text(encoding="utf-8") == "job addition\n"
    assert git(repo, "diff", "--cached", "--binary").stdout == before_index


def test_job_patch_stages_only_changed_paths(repo, tmp_path, monkeypatch):
    result = ledger.create_job_worktree(repo, "d" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 7\n", encoding="utf-8")
    (worktree / "job-new.txt").write_text("new\n", encoding="utf-8")
    original = ledger._run_git
    add_commands = []

    def recording_run_git(root, arguments, timeout, operation, **kwargs):
        if arguments[:2] == ["add", "-A"]:
            add_commands.append(list(arguments))
        return original(root, arguments, timeout, operation, **kwargs)

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)
    assert "src/target.py" in ledger.job_patch(worktree, result["snapshot"])
    assert len(add_commands) == 1
    assert add_commands[0][2] == "--"
    assert set(add_commands[0][3:]) == {":(literal)src/target.py", ":(literal)job-new.txt"}


def test_prepare_job_replay_uses_checkout_snapshot_without_touching_checkout(repo, tmp_path):
    (repo / "notes.txt").write_text("staged checkout\n", encoding="utf-8")
    git(repo, "add", "notes.txt")
    result = ledger.create_job_worktree(repo, "9" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 9\n", encoding="utf-8")
    (repo / "notes.txt").write_text("checkout edit\n", encoding="utf-8")
    (repo / "local.txt").write_text("untracked checkout file\n", encoding="utf-8")
    before_index = git(repo, "diff", "--cached", "--binary").stdout
    before_files = ((repo / "notes.txt").read_bytes(), (repo / "local.txt").read_bytes())
    assert before_index

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome["status"] == "prepared"
    assert (worktree / "notes.txt").read_text(encoding="utf-8") == "original\n"
    assert not (worktree / "local.txt").exists()
    prepared_patch = ledger.job_patch(worktree, outcome["snapshot"])
    assert prepared_patch.count("diff --git") == 1
    assert "notes.txt" not in prepared_patch
    assert git(repo, "diff", "--cached", "--binary").stdout == before_index
    assert ((repo / "notes.txt").read_bytes(), (repo / "local.txt").read_bytes()) == before_files
    assert (tmp_path / "recovery" / "worker-tree" / "src" / "target.py").read_text(encoding="utf-8") == "value = 9\n"


def test_prepare_job_replay_stages_only_job_paths(repo, tmp_path, monkeypatch):
    result = ledger.create_job_worktree(repo, "e" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 5\n", encoding="utf-8")
    (repo / "notes.txt").write_text("outside scope\n", encoding="utf-8")
    original = ledger._run_git
    add_commands = []

    def recording_run_git(root, arguments, timeout, operation, **kwargs):
        if arguments[:2] == ["add", "-A"]:
            add_commands.append(list(arguments))
        return original(root, arguments, timeout, operation, **kwargs)

    monkeypatch.setattr(ledger, "_run_git", recording_run_git)
    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")
    assert outcome["status"] == "prepared"
    assert len(add_commands) >= 2
    assert all(command[2:] == ["--", ":(literal)src/target.py"] for command in add_commands)
    assert (worktree / "notes.txt").read_text(encoding="utf-8") == "original\n"


def test_prepare_job_replay_skips_unchanged_checkout(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "c" * 64, tmp_path / "worktrees")
    outcome = ledger.prepare_job_replay(
        repo, result["path"], result["snapshot"], tmp_path / "recovery"
    )
    assert outcome == {"status": "unchanged", "snapshot": result["snapshot"], "paths": []}
    assert not (tmp_path / "recovery").exists()


def test_prepare_job_replay_new_file_with_moved_checkout(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "a" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "new.py").write_text("new = True\n", encoding="utf-8")
    (repo / "notes.txt").write_text("checkout edit\n", encoding="utf-8")

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome["status"] == "prepared"
    assert outcome["paths"] == ["src/new.py"]
    assert (worktree / "src" / "new.py").read_text(encoding="utf-8") == "new = True\n"
    assert "src/new.py" in ledger.job_patch(worktree, outcome["snapshot"])


def test_prepare_job_replay_allows_unrelated_dirty_checkout_paths(repo, tmp_path, monkeypatch):
    result = ledger.create_job_worktree(repo, "1" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "notes.txt").write_text("unrelated checkout edit\n", encoding="utf-8")
    monkeypatch.setattr(ledger, "_try_three_way_job_patch", lambda *_args, **_kwargs:
                        pytest.fail("directly applicable patch should not need a three-way merge"))

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome["status"] == "prepared"
    assert (worktree / "src" / "target.py").read_text(encoding="utf-8") == "value = 2\n"
    assert (worktree / "notes.txt").read_text(encoding="utf-8") == "original\n"


def test_prepare_job_replay_allows_reply_on_unapplied_thread_delta(repo, tmp_path, monkeypatch):
    result = ledger.create_job_worktree(repo, "2" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text(
        "value = 1\nthread_change = True\nreply_change = True\n", encoding="utf-8")
    (repo / "notes.txt").write_text("unrelated checkout edit\n", encoding="utf-8")
    monkeypatch.setattr(ledger, "_try_three_way_job_patch", lambda *_args, **_kwargs:
                        pytest.fail("the thread's applicable unapplied delta is not a conflict"))

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome["status"] == "prepared"
    assert (worktree / "src" / "target.py").read_text(encoding="utf-8") == (
        "value = 1\nthread_change = True\nreply_change = True\n")


def test_prepare_job_replay_still_reports_overlapping_checkout_change(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "3" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome == {"status": "conflict", "snapshot": result["snapshot"],
                       "paths": ["src/target.py"]}


def test_prepare_job_replay_blocks_conflict_and_preserves_worker_tree(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "a" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (worktree / "src" / "new.py").write_text("new = True\n", encoding="utf-8")
    original_patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")
    before_index = git(repo, "diff", "--cached", "--binary").stdout

    outcome = ledger.prepare_job_replay(repo, worktree, result["snapshot"], tmp_path / "recovery")

    assert outcome == {"status": "conflict", "snapshot": result["snapshot"],
                       "paths": ["src/target.py"]}
    assert ledger.job_patch(worktree, result["snapshot"]) == original_patch
    assert (tmp_path / "recovery" / "worker-tree" / "src" / "target.py").read_text(encoding="utf-8") == "value = 2\n"
    assert (repo / "src" / "target.py").read_text(encoding="utf-8") == "value = 3\n"
    assert git(repo, "diff", "--cached", "--binary").stdout == before_index


def test_prepare_job_replay_recovers_interrupted_preparation(repo, tmp_path, monkeypatch):
    result = ledger.create_job_worktree(repo, "b" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 9\n", encoding="utf-8")
    (repo / "notes.txt").write_text("checkout edit\n", encoding="utf-8")
    recovery = tmp_path / "recovery"
    original = ledger._try_three_way_job_patch
    original_run_git = ledger._run_git

    def require_three_way(*args, **kwargs):
        if args[1][0:2] == ["apply", "--check"]:
            raise ledger._InspectionError("GIT_FAILED", "simulated direct apply failure")
        return original_run_git(*args, **kwargs)

    def interrupted(*_args, **_kwargs):
        raise OSError("simulated interruption")

    monkeypatch.setattr(ledger, "_run_git", require_three_way)
    monkeypatch.setattr(ledger, "_try_three_way_job_patch", interrupted)
    with pytest.raises(OSError, match="simulated interruption"):
        ledger.prepare_job_replay(repo, worktree, result["snapshot"], recovery)
    assert json.loads((recovery / "state.json").read_text(encoding="utf-8"))["state"] == "PREPARING"
    assert (recovery / "worker-tree" / "src" / "target.py").read_text(encoding="utf-8") == "value = 9\n"

    monkeypatch.setattr(ledger, "_try_three_way_job_patch", original)
    resumed = ledger.prepare_job_replay(repo, worktree, result["snapshot"], recovery)
    assert resumed["status"] == "prepared"
    assert "notes.txt" not in ledger.job_patch(worktree, resumed["snapshot"])


def test_prepare_job_replay_accepts_resolved_conflict(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "d" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (worktree / "src" / "helper.py").write_text("helper = 2\n", encoding="utf-8")
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")
    calls = []

    def resolve(paths, target):
        calls.append((paths, target))
        assert (worktree / paths[0]).read_text(encoding="utf-8").count("<<<<<<<") == 1
        (worktree / paths[0]).write_text("value = 2\ncheckout = True\n", encoding="utf-8")
        return True

    outcome = ledger.prepare_job_replay(
        repo, worktree, result["snapshot"], tmp_path / "recovery", resolver=resolve
    )
    assert outcome["status"] == "prepared"
    assert outcome["resolved_conflicts"] == ["src/target.py"]
    assert calls and calls[0][0] == ["src/target.py"]
    assert calls[0][1] == outcome["snapshot"]
    prepared_patch = ledger.job_patch(worktree, outcome["snapshot"])
    assert "src/helper.py" in prepared_patch
    assert "helper = 2" in prepared_patch


def test_prepare_job_replay_rejects_resolver_markers(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "e" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    original_patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")

    outcome = ledger.prepare_job_replay(
        repo, worktree, result["snapshot"], tmp_path / "recovery",
        resolver=lambda _paths, _target: True,
    )
    assert outcome["status"] == "conflict"
    assert ledger.job_patch(worktree, result["snapshot"]) == original_patch


def test_prepare_job_replay_restores_after_resolver_timeout(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "f" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    original_patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("value = 3\n", encoding="utf-8")

    def timeout(_paths, _target):
        raise subprocess.TimeoutExpired("codex-reply", 60)

    outcome = ledger.prepare_job_replay(
        repo, worktree, result["snapshot"], tmp_path / "recovery", resolver=timeout
    )
    assert outcome["status"] == "conflict"
    assert ledger.job_patch(worktree, result["snapshot"]) == original_patch


def test_apply_job_patch_conflict_is_atomic_and_reports_path(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "c" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    patch = ledger.job_patch(worktree, result["snapshot"])

    (repo / "src" / "target.py").write_text("value = user\n", encoding="utf-8")
    before_tree = {path.relative_to(repo).as_posix(): path.read_bytes()
                   for path in repo.rglob("*") if path.is_file() and ".git" not in path.parts}
    before_index = git(repo, "diff", "--cached", "--binary").stdout
    applied = ledger.apply_job_patch(repo, patch)
    after_tree = {path.relative_to(repo).as_posix(): path.read_bytes()
                  for path in repo.rglob("*") if path.is_file() and ".git" not in path.parts}
    assert applied == {"applied": False, "conflicts": ["src/target.py"], "applicable": []}
    assert after_tree == before_tree
    assert git(repo, "diff", "--cached", "--binary").stdout == before_index


def test_failed_job_snapshot_removes_partial_worktree(repo, tmp_path, monkeypatch):
    parent = tmp_path / "worktrees"

    def fail_copy(*args, **kwargs):
        raise OSError("snapshot copy failed")

    monkeypatch.setattr(ledger, "_copy_untracked", fail_copy)
    with pytest.raises(OSError, match="snapshot copy failed"):
        ledger.create_job_worktree(repo, "d" * 64, parent)
    assert not (parent / ("d" * 12)).exists()
    listed = git(repo, "worktree", "list", "--porcelain").stdout
    assert str(parent / ("d" * 12)) not in listed


def test_apply_job_patch_reports_failed_and_applicable_paths(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "e" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    (worktree / "notes.txt").write_text("job note\n", encoding="utf-8")
    patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("value = user\n", encoding="utf-8")

    applied = ledger.apply_job_patch(repo, patch)

    assert applied == {"applied": False, "conflicts": ["src/target.py"],
                       "applicable": ["notes.txt"]}
    assert (repo / "notes.txt").read_text(encoding="utf-8") == "original\n"


def test_job_patch_excludes_gitignored_and_default_cache_paths(repo, tmp_path):
    (repo / ".gitignore").write_text("repo-cache/\n", encoding="utf-8")
    git(repo, "add", ".gitignore")
    git(repo, "commit", "-qm", "ignore repo cache")
    result = ledger.create_job_worktree(repo, "f" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    baseline = ledger.capture_baseline(worktree, ["src", "repo-cache", ".pytest_cache", "node_modules"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    for relative in ("repo-cache/new.dat", ".pytest_cache/v/cache", "src/__pycache__/x.pyc",
                     "node_modules/pkg/index.js", "src/module.pyc"):
        path = worktree / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("cache\n", encoding="utf-8")
    patch = ledger.job_patch(worktree, result["snapshot"])
    delta = ledger.filter_job_delta(worktree, ledger.compute_delta(worktree, baseline))

    assert "target.py" in patch
    assert delta["changed_paths"] == ["src/target.py"]
    for ignored in ("repo-cache", ".pytest_cache", "__pycache__", "node_modules", ".pyc"):
        assert ignored not in patch


def test_apply_job_patch_three_way_merges_nonoverlapping_change(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "1" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    source = (worktree / "src" / "target.py").read_text(encoding="utf-8")
    (worktree / "src" / "target.py").write_text(source + "job = True\n", encoding="utf-8")
    patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("user = True\n" + source, encoding="utf-8")

    applied = ledger.apply_job_patch(repo, patch)

    assert applied == {"applied": True, "conflicts": []}
    assert (repo / "src" / "target.py").read_text(encoding="utf-8") == "user = True\n" + source + "job = True\n"
    assert "<<<<<<<" not in (repo / "src" / "target.py").read_text(encoding="utf-8")


def test_apply_job_patch_three_way_conflict_leaves_tree_untouched(repo, tmp_path):
    result = ledger.create_job_worktree(repo, "2" * 64, tmp_path / "worktrees")
    worktree = Path(result["path"])
    (worktree / "src" / "target.py").write_text("value = 2\n", encoding="utf-8")
    patch = ledger.job_patch(worktree, result["snapshot"])
    (repo / "src" / "target.py").write_text("value = user\n", encoding="utf-8")
    before_tree = {p.relative_to(repo).as_posix(): p.read_bytes() for p in repo.rglob("*")
                   if p.is_file() and ".git" not in p.parts}

    applied = ledger.apply_job_patch(repo, patch)

    after_tree = {p.relative_to(repo).as_posix(): p.read_bytes() for p in repo.rglob("*")
                  if p.is_file() and ".git" not in p.parts}
    assert applied == {"applied": False, "conflicts": ["src/target.py"], "applicable": []}
    assert after_tree == before_tree
