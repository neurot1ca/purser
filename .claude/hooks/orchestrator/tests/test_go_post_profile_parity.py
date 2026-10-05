import json
import os
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "post_profile.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex
import project_profile


def _valid_candidates(value):
    return value is None or isinstance(value, list) and all(isinstance(item, dict)
        and isinstance(item.get("agent"), str) and isinstance(item.get("model"), str) for item in value)
def _profile(pending, config):
    stored = (
        isinstance(pending.get("protected_paths"), list)
        and all(isinstance(path, str) for path in pending["protected_paths"])
        and isinstance(pending.get("force_full_review"), bool)
        and all(type(pending.get(key)) is int for key in ("skip_max_lines", "skip_max_files"))
        and isinstance(pending.get("configured"), bool)
        and isinstance(pending.get("user_status"), str) and isinstance(pending.get("project_status"), str)
        and _valid_candidates(pending.get("reviewer_candidates"))
    )
    if not stored:
        return {
            "stored": False, "protected_paths": list(project_profile._ENGINE_PROTECTED), "force_full_review": True,
            "skip_max_lines": None, "skip_max_files": None, "reviewer_executable": None, "reviewer": "agy",
            "reviewer_candidates": [{"agent": "agy", "model": config["reviewer1_model_requested"]}],
            "authority_sources": None, "configured": None, "user_status": None, "project_status": None, "first_warning": None,
        }
    return {
        "stored": True,
        "protected_paths": pending["protected_paths"], "force_full_review": pending["force_full_review"],
        "skip_max_lines": pending["skip_max_lines"], "skip_max_files": pending["skip_max_files"],
        "reviewer_executable": pending.get("reviewer_executable"),
        "reviewer": pending.get("reviewer", "agy"),
        "reviewer_candidates": pending.get("reviewer_candidates") or [{"agent": "agy", "model": config["reviewer1_model_requested"]}],
        "authority_sources": pending.get("authority_sources"),
        "configured": pending["configured"], "user_status": pending["user_status"],
        "project_status": pending["project_status"],
        "first_warning": pending.get("first_warning"),
    }
def _outcome(bundle, result, protected_paths):
    bundle = json.loads(json.dumps(bundle))
    return bundle, codex._apply_out_of_scope_result(bundle, result, protected_paths=protected_paths)
def _fixture():
    config = {"reviewer1_model_requested": "gemini-3.8-flash-high"}
    full = {
        "protected_paths": ["src/private/**"], "force_full_review": False,
        "skip_max_lines": 12, "skip_max_files": 3, "configured": True,
        "user_status": "ok", "project_status": "ok",
        "reviewer_executable": "agy", "reviewer": "claude", "reviewer_candidates": [
            {"agent": "claude", "model": "sonnet"}, {"agent": "agy", "model": "flash"}
        ], "authority_sources": [{"name": "team"}], "first_warning": "warning",
    }
    empty_candidates = {**full, "reviewer_candidates": []}
    invalid = {**full, "protected_paths": ["src/**", 5]}
    profiles = [
        {"name": "full_stored_profile", "pending": full, "config": config},
        {"name": "invalid_field_falls_back", "pending": invalid, "config": config},
        {"name": "empty_candidates_use_default", "pending": empty_candidates, "config": config},
    ]
    for case in profiles:
        case["expected"] = _profile(case["pending"], case["config"])

    protected = (["src/**", ".claude/settings.json"], {"status": "OK", "paths": ["src/a.go", ".claude/settings.json"]}, {"findings": [], "protected_paths_touched": []})
    unprotected = (["private/**"], {"status": "OK", "paths": ["src/a.go"]}, {"findings": []})
    unknown = ([], {"status": "UNKNOWN", "code": "UNKNOWN_OUT_OF_SCOPE", "message": "scope evidence unavailable"}, {"findings": []})
    many = ([], {"status": "OK", "paths": [f"src/file-{i}.go" for i in range(7)]}, {"findings": []})
    merged = ([".claude/settings.json"], {"status": "OK", "paths": [".claude/settings.json"]}, {"findings": [], "protected_paths_touched": ["z.txt", "z.txt"]})
    outs = []
    for name, (patterns, result, bundle) in zip(
        ["protected_paths", "no_protected_paths", "unknown_status", "truncate_message", "merge_existing_protected_paths"],
        [protected, unprotected, unknown, many, merged],
    ):
        expected_bundle, force_full = _outcome(bundle, result, patterns)
        outs.append({"name": name, "protected_paths": patterns, "result": result, "bundle": bundle,
                     "expected_bundle": expected_bundle, "force_full": force_full})
    return {"oracle": "Python PostToolUse stored_profile intake and _apply_out_of_scope_result", "profiles": profiles, "out_of_scope": outs}
def test_go_post_profile_oracle_fixture():
    expected = _fixture()
    if os.environ.get("PURSER_WRITE_GO_POST_PROFILE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set PURSER_WRITE_GO_POST_PROFILE_FIXTURE=1 to regenerate)"
    actual = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert actual == expected
