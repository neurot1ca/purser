import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "replay.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger

CASES = [
    {"name": "unchanged"},
    {"name": "fresh_prepared_direct_apply", "target_changes": [{"path": "other.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}]},
    {"name": "prior_prepared", "seed": "PREPARED"},
    {"name": "prior_conflict", "seed": "CONFLICT"},
    {"name": "prior_blocked", "seed": "BLOCKED"},
    {"name": "prior_resolving", "seed": "RESOLVING"},
    {"name": "unknown_state", "seed": "UNKNOWN"},
    {"name": "unreadable_state", "seed": "UNREADABLE"},
    {"name": "resumed_preparing", "seed": "PREPARING", "target_changes": [{"path": "other.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}]},
    {"name": "empty_patch", "target_changes": [{"path": "other.txt", "content": "target\n"}]},
    {"name": "three_way_success", "base_tracked": "alpha\nmiddle\nomega\n", "target_changes": [{"path": "tracked.txt", "content": "TARGET\nmiddle\nomega\n"}], "worker_changes": [{"path": "tracked.txt", "content": "alpha\nmiddle\nWORKER\n"}]},
    {"name": "three_way_conflict", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}]},
    {"name": "resolver_clean", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}], "resolver": "clean"},
    {"name": "resolver_markers_remain", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}], "resolver": "markers"},
    {"name": "resolver_raises", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}], "resolver": "raises"},
    {"name": "new_path_excluded", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}, {"path": "new.txt", "content": "new\n"}]},
    {"name": "ls_tree_inspection_error", "target_changes": [{"path": "tracked.txt", "content": "target\n"}], "worker_changes": [{"path": "tracked.txt", "content": "worker\n"}], "script": "ls_tree_error"},
]


def _git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _output(root, *args):
    return subprocess.run(["git", *args], cwd=root, check=True, capture_output=True, text=True).stdout.strip()


def _repo(root, case):
    root.mkdir(parents=True)
    _git(root, "init", "-q")
    for name, content in {"tracked.txt": case.get("base_tracked", "base\n").encode(), "other.txt": b"other\n"}.items():
        (root / name).write_bytes(content)
    _git(root, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture"], cwd=root, env=env, check=True, capture_output=True)
    return _output(root, "rev-parse", "HEAD^{tree}")


def _changes(root, changes):
    for change in changes:
        path = root / change["path"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(change["content"], encoding="utf-8", newline="\n")


def _copy_worker_tree(source, destination):
    shutil.copytree(source, destination, ignore=shutil.ignore_patterns(".git"), symlinks=True, dirs_exist_ok=True)


def _normalize(value, snapshot, target):
    if isinstance(value, dict):
        return {key: _normalize(item, snapshot, target) for key, item in value.items()}
    if isinstance(value, list):
        return [_normalize(item, snapshot, target) for item in value]
    if value == target:
        return "<TARGET>"
    if value == snapshot:
        return "<SNAPSHOT>"
    return value


def _case(case, root):
    repo, worker, recovery = root / "repo", root / "worker", root / "recovery"
    snapshot = _repo(repo, case)
    _git(repo, "config", "core.autocrlf", "false")
    _git(repo, "worktree", "add", "--detach", str(worker.resolve()), "HEAD")
    _changes(repo, case.get("target_changes", []))
    _changes(worker, case.get("worker_changes", []))
    job_paths = [change["path"] for change in case.get("worker_changes", [])]
    target = ledger.checkout_snapshot(repo, job_paths)
    seed = case.get("seed")
    if seed:
        recovery.mkdir()
        if seed == "UNREADABLE":
            (recovery / "state.json").write_text("{bad json", encoding="utf-8")
        else:
            state = {"state": seed, "snapshot": snapshot, "target": target, "paths": ["tracked.txt"], "conflicts": ["tracked.txt"]}
            if seed == "PREPARED": state["resolved_conflicts"] = []
            if seed == "PREPARING":
                state.pop("conflicts")
                (recovery / "worker.patch").write_bytes(ledger.job_patch(worker, snapshot).encode("utf-8", "surrogateescape"))
                _copy_worker_tree(worker, recovery / "worker-tree")
            if seed == "RESOLVING": _copy_worker_tree(worker, recovery / "worker-tree")
            (recovery / "state.json").write_text(json.dumps(state, separators=(",", ":")), encoding="utf-8")
    original_run_git = ledger._run_git
    if case.get("script") == "ls_tree_error":
        def scripted_run_git(root, arguments, timeout, operation, **kwargs):
            if operation == "checking replay source path":
                raise ledger._InspectionError("GIT_FAILED", "scripted ls-tree inspection failure")
            return original_run_git(root, arguments, timeout, operation, **kwargs)
        ledger._run_git = scripted_run_git
    def resolver(conflicts, current):
        if case["resolver"] == "raises":
            raise RuntimeError("scripted resolver failure")
        if case["resolver"] == "clean":
            (worker / "tracked.txt").write_text("resolved\n", encoding="utf-8")
        elif case["resolver"] == "markers":
            (worker / "tracked.txt").write_text("<<<<<<< ours\n=======\n>>>>>>> theirs\n", encoding="utf-8")
        return True
    try:
        result = ledger.prepare_job_replay(repo, worker, snapshot, recovery,
                                           resolver=resolver if case.get("resolver") else None)
        error = None
    except ledger._InspectionError as exc:
        result, error = None, {"code": exc.code, "message": exc.message}
    normalization_target = target
    if target == snapshot and ((result or {}).get("status") != "prepared" or seed == "PREPARED"):
        normalization_target = ""
    state = None
    state_path = recovery / "state.json"
    if state_path.exists():
        try:
            state = _normalize(json.loads(state_path.read_text(encoding="utf-8")), snapshot, normalization_target)
        except (OSError, ValueError):
            state = "<UNREADABLE>"
    tree = []
    worker_tree = recovery / "worker-tree"
    if worker_tree.exists():
        for path in sorted(worker_tree.rglob("*")):
            if path.is_file():
                tree.append({"path": path.relative_to(worker_tree).as_posix(), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()})
            elif path.is_symlink():
                tree.append({"path": path.relative_to(worker_tree).as_posix(), "symlink": os.readlink(path)})
    ledger._run_git = original_run_git
    return {**case, "result": _normalize(result, snapshot, normalization_target), "error": error,
            "recovery": {"files": sorted(path.name for path in recovery.iterdir()) if recovery.exists() else [],
                         "state": state, "worker_patch": (recovery / "worker.patch").exists(), "worker_tree": tree}}


def test_python_oracle_replay_fixture(tmp_path):
    generated = {"oracle": "Python ledger.prepare_job_replay", "cases": [_case(case, tmp_path / str(i)) for i, case in enumerate(CASES)]}
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == generated
