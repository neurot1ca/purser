import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).parents[1]
BRIDGE = ROOT / "reviewer_bridge.py"
sys.path.insert(0, str(ROOT))
_CHILD = r'''
import os, runpy, sys
import reviewer
from reviewer import CompletedReview, IncompleteReview, UnavailableReview
case = os.environ["BRIDGE_CASE"]
if case == "raise":
    reviewer.review = lambda **kwargs: (_ for _ in ()).throw(RuntimeError("stub failure"))
elif case == "completed":
    reviewer.review = lambda **kwargs: CompletedReview(
        result="NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", validation="PASS",
        findings=(("F1", "finding"),), conversation_id="conversation",
        reviewer1_model_requested="review-model", duration_seconds=1.5,
        usage=(("input_tokens", 3),), provider="codex",
        served_model_attestation="ATTESTED", retry_count=2, retry_recovered=True)
elif case == "unavailable":
    reviewer.review = lambda **kwargs: UnavailableReview(
        error_code="NO_RESPONSE", message="unavailable", reviewer1_model_requested="review-model",
        duration_seconds=2.5, usage=(("input_tokens", 4),), raw_stdout="raw",
        decoder_detail="decode", elapsed_seconds=2.0, stderr_tail="stderr",
        provider="opencode", retry_count=1, retry_recovered=False)
else:
    reviewer.review = lambda **kwargs: IncompleteReview(
        code="PACKET_INCOMPLETE", message="omitted", omitted=("diff",),
        reviewer1_model_requested="review-model", provider="agy")
runpy.run_path(sys.argv[1], run_name="__main__")
'''


def _input():
    return {
        "task_id": "task", "thread_id": "thread", "project_key": "project",
        "state_root": "C:/state", "phase": "implementation", "route": "normal",
        "model": "coder", "effort": "high", "repo_root": "C:/repo",
        "delta": {"changed_paths": ["file.py"]}, "classification": {"in_scope_paths": ["file.py"]},
        "validation_summary": "PASS", "task_objective": "objective",
        "acceptance_criteria": "criteria", "reviewer_executable": "reviewer.exe",
        "authority_sources": ["AGENTS.md"], "reviewer_agent": "agy",
        "reviewer_model": "review-model", "reviewer_timeout_seconds": 8.5,
    }


def _run(payload, case="incomplete"):
    child_env = dict(os.environ, BRIDGE_CASE=case)
    return subprocess.run(
        [sys.executable, "-c", _CHILD, str(BRIDGE)], input=payload,
        text=True, capture_output=True, env=child_env, cwd=ROOT, check=False,
    )


def test_completed_outcome_round_trips_every_post_mapping_attribute():
    from reviewer import CompletedReview

    expected = CompletedReview(
        result="NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", validation="PASS",
        findings=(("F1", "finding"),), conversation_id="conversation",
        reviewer1_model_requested="review-model", duration_seconds=1.5,
        usage=(("input_tokens", 3),), provider="codex",
        served_model_attestation="ATTESTED", retry_count=2, retry_recovered=True)
    proc = _run(json.dumps(_input()), "completed")
    assert proc.returncode == 0
    assert proc.stderr == ""
    assert proc.stdout == json.dumps({"outcome": "completed", **dict(expected)}, separators=(",", ":")) + "\n"


def test_unavailable_outcome():
    proc = _run(json.dumps(_input()), "unavailable")
    assert proc.returncode == 0
    assert json.loads(proc.stdout) == {
        "outcome": "unavailable", "review_status": "UNAVAILABLE", "available": False,
        "provider": "opencode", "result": "REVIEWER1_UNAVAILABLE", "validation": "UNKNOWN",
        "findings": [{"code": "NO_RESPONSE", "message": "unavailable"}],
        "error_code": "NO_RESPONSE", "reviewer1_model_requested": "review-model",
        "served_model_attestation": "UNAVAILABLE",
    }


def test_incomplete_outcome():
    proc = _run(json.dumps(_input()))
    assert proc.returncode == 0
    assert json.loads(proc.stdout) == {
        "outcome": "incomplete", "review_status": "NOT_RUN_INCOMPLETE_PACKET",
        "available": True, "provider": "agy", "result": "UNCERTAIN",
        "validation": "UNKNOWN", "findings": [{"code": "PACKET_INCOMPLETE", "message": "omitted"}],
        "omitted": ["diff"], "reviewer1_model_requested": "review-model",
        "served_model_attestation": "UNAVAILABLE",
    }


def test_raising_reviewer_is_unavailable():
    proc = _run(json.dumps(_input()), "raise")
    output = json.loads(proc.stdout)
    assert proc.returncode == 0
    assert output["outcome"] == "unavailable"
    assert output["reason"] == output["error_code"] == "REVIEWER_BRIDGE_EXCEPTION"
    assert output["result"] == "REVIEWER1_UNAVAILABLE"
    assert output["validation"] == "UNKNOWN"


def test_malformed_stdin_is_structured_error():
    proc = _run("{")
    assert proc.returncode != 0
    assert json.loads(proc.stdout)["error"] == "INVALID_INPUT"


def test_unknown_key_is_rejected():
    payload = _input() | {"surprise": True}
    proc = _run(json.dumps(payload))
    assert proc.returncode != 0
    assert json.loads(proc.stdout)["error"] == "INVALID_INPUT"


def test_non_object_json_is_rejected():
    proc = _run("[]")
    assert proc.returncode != 0
    assert json.loads(proc.stdout)["error"] == "INVALID_INPUT"


def test_wrong_type_is_rejected():
    payload = _input() | {"delta": []}
    proc = _run(json.dumps(payload))
    assert proc.returncode != 0
    assert json.loads(proc.stdout)["error"] == "INVALID_INPUT"
