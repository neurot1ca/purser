import json
import os
from pathlib import Path
import subprocess
import sys
import shutil
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "delta.json"
COMPUTE_FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "compute_delta.json"
OUT_OF_SCOPE_FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "out_of_scope.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger

CASES = [
    {"name": "names", "kind": "names", "raw": "dir\\old.txt\0same\0same\0\0"},
    {"name": "numstat", "kind": "numstat", "raw": "2\t3\tdir\\old.txt\0-\t-\tbinary\0" "1\t0\tdir/old.txt\0"},
    {"name": "numstat_columns", "kind": "numstat", "raw": "bad\0"},
    {"name": "numstat_counts", "kind": "numstat", "raw": "x\t1\tbad.txt\0"},
    {"name": "manifest", "kind": "manifest", "baseline": [
        {"path": "changed", "sha256": "a", "line_count": 2},
        {"path": "removed", "sha256": "b", "line_count": 4},
        {"path": "same", "sha256": "c", "line_count": 9},
    ], "current": [
        {"path": "changed", "sha256": "d", "line_count": 3},
        {"path": "added", "sha256": "e", "line_count": 0},
        {"path": "same", "sha256": "c", "line_count": "invalid"},
    ]},
    {"name": "manifest_counts", "kind": "manifest", "baseline": [], "current": [
        {"path": "bad", "sha256": "a", "line_count": "invalid"},
    ]},
]


def _oracle(case):
    try:
        if case["kind"] == "names":
            result = {"paths": sorted(ledger._parse_name_list(case["raw"].encode()))}
        elif case["kind"] == "numstat":
            result = {"counts": ledger._parse_numstat(case["raw"].encode())}
        else:
            paths, counts = ledger._manifest_delta(case["baseline"], case["current"])
            result = {"paths": sorted(paths), "counts": counts}
        return {**case, "result": result}
    except ledger._InspectionError as error:
        return {**case, "error": {"code": error.code, "message": error.message}}


def test_python_oracle_delta_fixture():
    generated = {
        "oracle": "Python ledger._parse_name_list, _parse_numstat, and _manifest_delta",
        "cases": [_oracle(case) for case in CASES],
    }
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == generated


COMPUTE_CASES = [
    {"name": "clean", "changes": []},
    {"name": "in_scope_modification", "changes": [{"op": "write", "path": "scope/tracked.txt", "content": "changed\n"}]},
    {"name": "out_of_scope_untracked", "changes": [{"op": "write", "path": "outside/new.txt", "content": "ignored by scope\n"}]},
    {"name": "untracked_file", "changes": [{"op": "write", "path": "scope/new.txt", "content": "new\n"}]},
    {"name": "deleted_file", "changes": [{"op": "delete", "path": "scope/delete.txt"}]},
    {"name": "missing_baseline", "baseline_override": None},
    {"name": "stale_baseline_head", "baseline_head": "missing-baseline-head"},
    {"name": "scope_none", "scope": None, "baseline_scope": ["scope"], "changes": []},
    {"name": "integral_float_manifest_count", "scope": ["scope"], "manifest_count": 1},
    {"name": "nonintegral_float_manifest_count", "scope": ["scope"], "manifest_count": 1.5},
]


def _git(root, *args, env=None):
    subprocess.run(["git", *args], cwd=root, env=env, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _compute_oracle(case, root):
    _git(root, "init", "-q")
    for name, content in {
        "scope/tracked.txt": "base\n",
        "scope/delete.txt": "delete me\n",
        "outside/tracked.txt": "outside\n",
    }.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content.encode("utf-8"))
    _git(root, "add", "--all")
    env = os.environ.copy()
    env.update({"GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"})
    _git(root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture", env=env)
    baseline = ledger.capture_baseline(root, case.get("baseline_scope", ["scope"]))
    if "baseline_override" in case:
        baseline = case["baseline_override"]
    if "baseline_head" in case:
        baseline["head"] = case["baseline_head"]
    if "manifest_count" in case:
        baseline["scoped_manifest"].append({"path": "scope/removed.txt", "sha256": "baseline-only", "line_count": case["manifest_count"]})
    for change in case.get("changes", []):
        path = root / change["path"]
        if change["op"] == "delete":
            path.unlink()
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(change["content"].encode("utf-8"))
    result = ledger.compute_delta(root, baseline, case.get("scope"))
    return {"name": case["name"], "scope": case.get("scope"), "changes": case.get("changes", []), "baseline": baseline, "result": result}


def test_python_oracle_compute_delta_fixture():
    generated = {"oracle": "Python ledger.compute_delta", "cases": []}
    scratch = ROOT / ".tmp"
    scratch.mkdir(exist_ok=True)
    temporary = scratch / ("compute-delta-" + uuid.uuid4().hex)
    temporary.mkdir()
    try:
        for index, case in enumerate(COMPUTE_CASES):
            root = temporary / str(index)
            root.mkdir()
            generated["cases"].append(_compute_oracle(case, root))
    finally:
        shutil.rmtree(temporary, ignore_errors=True)
    assert json.loads(COMPUTE_FIXTURE.read_text(encoding="utf-8")) == generated


OUT_OF_SCOPE_CASES = [
    {"name": "clean", "changes": []},
    {"name": "in_scope_modification", "changes": [{"path": "scope/tracked.txt", "content": "changed\n"}]},
    {"name": "out_of_scope_tracked", "changes": [{"path": "outside/tracked.txt", "content": "changed\n"}]},
    {"name": "out_of_scope_untracked", "changes": [{"path": "outside/new.txt", "content": "new\n"}]},
    {"name": "preexisting_dirty_unchanged", "pre_changes": [{"path": "outside/tracked.txt", "content": "dirty\n"}]},
    {"name": "preexisting_dirty_mutated", "pre_changes": [{"path": "outside/tracked.txt", "content": "staged\n"}], "stage_before_baseline": ["outside/tracked.txt"], "changes": [{"path": "outside/tracked.txt", "content": "mutated\n"}]},
    {"name": "invalid_baseline", "baseline_override": None},
    {"name": "no_scope", "scope": []},
    {"name": "invalid_scope", "scope": ["../escape"]},
    {"name": "committed_out_of_scope", "changes": [{"path": "outside/tracked.txt", "content": "committed\n"}], "commit_changes": True},
]


def _out_of_scope_oracle(case, root):
    _git(root, "init", "-q")
    for name, content in {"scope/tracked.txt": "base\n", "outside/tracked.txt": "outside\n"}.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content.encode("utf-8"))
    _git(root, "add", "--all")
    env = os.environ.copy()
    env.update({"GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"})
    _git(root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture", env=env)
    for change in case.get("pre_changes", []):
        path = root / change["path"]
        path.write_bytes(change["content"].encode("utf-8"))
    if case.get("stage_before_baseline"):
        _git(root, "add", *case["stage_before_baseline"])
    baseline = ledger.capture_baseline(root, ["scope"])
    if "baseline_override" in case:
        baseline = case["baseline_override"]
    for change in case.get("changes", []):
        path = root / change["path"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(change["content"].encode("utf-8"))
    if case.get("commit_changes"):
        _git(root, "add", "--all")
        _git(root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "after baseline", env=env)
    result = ledger.detect_out_of_scope_modifications(root, baseline, case.get("scope", ["scope"]))
    return {"name": case["name"], "scope": case.get("scope", ["scope"]), "pre_changes": case.get("pre_changes", []), "stage_before_baseline": case.get("stage_before_baseline", []), "changes": case.get("changes", []), "commit_changes": case.get("commit_changes", False), "baseline": baseline, "result": result}


def test_python_oracle_out_of_scope_fixture():
    generated = {"oracle": "Python ledger.detect_out_of_scope_modifications", "cases": []}
    scratch = ROOT / ".tmp"
    scratch.mkdir(exist_ok=True)
    temporary = scratch / ("out-of-scope-" + uuid.uuid4().hex)
    temporary.mkdir()
    try:
        for index, case in enumerate(OUT_OF_SCOPE_CASES):
            root = temporary / str(index)
            root.mkdir()
            generated["cases"].append(_out_of_scope_oracle(case, root))
    finally:
        shutil.rmtree(temporary, ignore_errors=True)
    assert json.loads(OUT_OF_SCOPE_FIXTURE.read_text(encoding="utf-8")) == generated
