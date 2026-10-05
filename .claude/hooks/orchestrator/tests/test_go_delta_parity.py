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
ATTRIBUTION_FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "attribution.json"
CONCURRENT_WRITERS_FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "concurrent_writers.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger
import project_profile

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

ATTRIBUTION_CASES = [
    {"name": "single_foreign_owner", "delta": {"status": "OK", "changed_paths": ["foreign/a.txt"], "changed_file_count": 1, "changed_line_count": 4, "path_line_counts": {"foreign/a.txt": 4}, "sources": {"staged": {"paths": ["foreign/a.txt"], "changed_line_count": 4, "path_line_counts": {"foreign/a.txt": 4}}}, "ambiguous_paths": []}, "owners": [{"task_id": "task-a", "scope": ["foreign"]}]},
    {"name": "two_foreign_owners", "delta": {"status": "OK", "changed_paths": ["shared/a.txt"], "changed_file_count": 1, "changed_line_count": 2, "path_line_counts": {"shared/a.txt": 2}, "sources": {}, "ambiguous_paths": []}, "owners": [{"task_id": "task-a", "scope": ["shared"]}, {"task_id": "task-b", "scope": ["shared"]}]},
    {"name": "ambiguous_path", "delta": {"status": "OK", "changed_paths": ["foreign/a.txt"], "changed_file_count": 1, "changed_line_count": 2, "path_line_counts": {"foreign/a.txt": 2}, "sources": {}, "ambiguous_paths": ["foreign/a.txt"]}, "owners": [{"task_id": "task-a", "scope": ["foreign"]}]},
    {"name": "default_protected_path", "delta": {"status": "OK", "changed_paths": [".env"], "changed_file_count": 1, "changed_line_count": 1, "path_line_counts": {".env": 1}, "sources": {}, "ambiguous_paths": []}, "owners": [{"task_id": "task-a", "scope": ["."]}]},
    {"name": "explicit_protected_path", "delta": {"status": "OK", "changed_paths": ["private/key.txt"], "changed_file_count": 1, "changed_line_count": 1, "path_line_counts": {"private/key.txt": 1}, "sources": {}, "ambiguous_paths": []}, "owners": [{"task_id": "task-a", "scope": ["private"]}], "protected_paths": ["private/**"]},
    {"name": "inside_own_scope", "delta": {"status": "OK", "changed_paths": ["mine/a.txt"], "changed_file_count": 1, "changed_line_count": 3, "path_line_counts": {"mine/a.txt": 3}, "sources": {}, "ambiguous_paths": []}, "own_scope": ["mine"], "owners": [{"task_id": "task-a", "scope": ["mine"]}]},
    {"name": "additional_path", "delta": {"status": "OK", "changed_paths": [], "changed_file_count": 0, "changed_line_count": 0, "path_line_counts": {}, "sources": {}, "ambiguous_paths": []}, "owners": [{"task_id": "task-a", "scope": ["foreign"]}], "additional_paths": ["foreign/new.txt"]},
    {"name": "non_ok_delta", "delta": {"status": "UNKNOWN", "code": "DELTA_FAILED", "changed_paths": ["foreign/a.txt"]}, "owners": [{"task_id": "task-a", "scope": ["foreign"]}]},
    {"name": "multi_source_line_recount", "delta": {"status": "OK", "changed_paths": ["foreign/a.txt", "mine/b.txt"], "changed_file_count": 2, "changed_line_count": 9, "path_line_counts": {"foreign/a.txt": 5, "mine/b.txt": 4}, "sources": {"staged": {"paths": ["foreign/a.txt", "mine/b.txt"], "changed_line_count": 7, "path_line_counts": {"foreign/a.txt": 3, "mine/b.txt": 4}}, "unstaged": {"paths": ["foreign/a.txt"], "changed_line_count": 2, "path_line_counts": {"foreign/a.txt": 2}}}, "ambiguous_paths": []}, "own_scope": ["mine"], "owners": [{"task_id": "task-a", "scope": ["foreign"]}]},
]

def test_python_oracle_attribution_fixture():
    generated = {"oracle": "Python ledger.attribute_known_concurrent_scopes and detect_protected_paths", "engine_protected": list(project_profile._ENGINE_PROTECTED), "cases": []}
    for case in ATTRIBUTION_CASES:
        patterns = case.get("protected_paths")
        filtered, known = ledger.attribute_known_concurrent_scopes(
            case["delta"], case.get("own_scope", []), case.get("owners", []),
            protected_paths=patterns, additional_paths=case.get("additional_paths", []),
        )
        generated["cases"].append({**case, "protected": ledger.detect_protected_paths(case["delta"], protected_paths=patterns), "result": {"delta": filtered, "known": known}})
    assert json.loads(ATTRIBUTION_FIXTURE.read_text(encoding="utf-8")) == generated


CONCURRENT_WRITER_CASES = [
    {"name": "non_ok_delta", "delta": {"status": "UNKNOWN"}, "scope": ["scope"]},
    {"name": "invalid_scope", "delta": {"status": "OK", "changed_paths": []}, "scope": ["../escape"]},
    {"name": "all_in_scope", "delta": {"status": "OK", "changed_paths": ["scope/a", "scope/b"], "path_line_counts": {"scope/a": 2, "scope/b": 3}}, "scope": ["scope"]},
    {"name": "out_of_scope_no_halt", "delta": {"status": "OK", "changed_paths": ["outside/a"], "path_line_counts": {}}, "scope": ["scope"]},
    {"name": "in_scope_ambiguous_overlap", "delta": {"status": "OK", "changed_paths": ["scope/a"], "ambiguous_paths": ["scope/a"], "path_line_counts": {}}, "scope": ["scope"]},
    {"name": "material_path_inside", "delta": {"status": "OK", "changed_paths": ["outside/build/a"], "path_line_counts": {}}, "scope": ["scope"], "material_paths": ["outside/build"]},
    {"name": "material_path_contains", "delta": {"status": "OK", "changed_paths": ["outside/vendor"], "path_line_counts": {}}, "scope": ["scope"], "material_paths": ["outside/vendor/generated/file"]},
    {"name": "verification_path", "delta": {"status": "OK", "changed_paths": ["outside/check.py"], "path_line_counts": {}}, "scope": ["scope"], "verification_paths": ["outside/check.py"]},
    {"name": "protected_touch", "delta": {"status": "OK", "changed_paths": [".env"], "path_line_counts": {}}, "scope": ["scope"]},
    {"name": "preexisting_outside_scope", "delta": {"status": "OK", "changed_paths": ["scope/a"], "preexisting_paths": ["outside/old"], "path_line_counts": {"scope/a": 1}}, "scope": ["scope"]},
]


def test_python_oracle_concurrent_writers_fixture():
    generated = {"oracle": "Python ledger.classify_concurrent_writers", "cases": []}
    for case in CONCURRENT_WRITER_CASES:
        result = ledger.classify_concurrent_writers(
            case["delta"], case["scope"], material_paths=case.get("material_paths", []),
            verification_paths=case.get("verification_paths", []),
        )
        generated["cases"].append({**case, "result": result})
    assert json.loads(CONCURRENT_WRITERS_FIXTURE.read_text(encoding="utf-8")) == generated
