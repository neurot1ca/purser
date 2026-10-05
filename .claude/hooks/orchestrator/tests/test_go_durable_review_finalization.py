"""Python oracle for durable async review finalization."""
import json
import os
from pathlib import Path

import test_go_post_async_success_parity as success
import codex

FIXTURE = success.ORCH / "tests" / "fixtures" / "go_policy" / "durable_review_finalization.json"


def _run(root, monkeypatch, *, reviewer=None):
    root.mkdir()
    state, repo, event, index, pending = success._success(root)
    (repo / "src" / "file.go").write_text("package src\n// review delta\n", encoding="utf-8")
    config = json.loads((success.ORCH / "config.json").read_text(encoding="utf-8"))
    config["reviewer_gate"] = {"small_delta_max_files": 0, "small_delta_max_lines": 0}
    response = codex._async_post_followup(event, config=config, state_root=state, repo_root=repo,
                                          reviewer=reviewer)
    bundle = json.loads(response["hookSpecificOutput"]["updatedToolOutput"][0]["text"])
    stored = json.loads((state / "async_jobs" / f"{index['jobId']}.json").read_text(encoding="utf-8"))
    artifact = json.loads((state / "bundle_artifacts" / f"{index['call_key']}.json").read_text(encoding="utf-8"))
    return state, repo, event, config, index, pending, response, {
        "mode": bundle.get("claude_review_mode"), "reviewer_result": bundle.get("reviewer1_result"),
        "reviewer_skipped_reason": bundle.get("reviewer1_skipped_reason"),
        "stage": stored.get("stage"), "bundle_persisted": bool(artifact),
    }


def test_durable_review_finalization_python_oracle(tmp_path, monkeypatch):
    calls = []

    def reviewer(**_kwargs):
        calls.append(1)
        return {"review_status": "COMPLETED", "provider": "agy", "reviewer1_model_requested": "gemini-3.8-flash-high",
                "result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []}

    normal = _run(tmp_path / "normal", monkeypatch, reviewer=reviewer)[-1]
    normal["reviewer_calls"] = len(calls)

    calls.clear()
    recovery_state, recovery_repo, recovery_event, recovery_config, recovery_index, pending, first_response, recovery = _run(
        tmp_path / "recovery", monkeypatch, reviewer=reviewer)
    recovery_path = recovery_state / "async_jobs" / f"{recovery_index['jobId']}.json"
    recovered_index = json.loads(recovery_path.read_text(encoding="utf-8"))
    recovered_index["stage"] = "FINALIZING"
    recovery_path.write_text(json.dumps(recovered_index), encoding="utf-8")
    (recovery_state / "pending" / f"{recovery_index['call_key']}.json").write_text(
        json.dumps(pending), encoding="utf-8")
    first_calls = len(calls)
    retry = codex._async_post_followup(recovery_event, config=recovery_config, state_root=recovery_state,
                                        repo_root=recovery_repo, reviewer=reviewer)
    after_retry = len(calls)
    repeated = codex._async_post_followup(recovery_event, config=recovery_config, state_root=recovery_state,
                                          repo_root=recovery_repo, reviewer=reviewer)
    recovery["reviewer_calls"] = first_calls
    recovery["retry_response_reused"] = retry == first_response
    recovery["retry_review_delta"] = after_retry - first_calls
    recovery["second_result_reused"] = repeated == retry
    recovery["second_result_review_delta"] = len(calls) - after_retry

    def unavailable(**_kwargs):
        raise RuntimeError("reviewer bridge unavailable")

    failed = _run(tmp_path / "unavailable", monkeypatch, reviewer=unavailable)[-1]
    fixture = {"oracle": "codex._async_post_followup durable async review finalization",
               "cases": {"normal": normal, "finalizing_recovery": recovery,
                         "reviewer_bridge_unavailable": failed}}
    if os.environ.get("PURSER_REGEN_FIXTURES") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.is_file(), f"missing fixture: {FIXTURE} (set PURSER_REGEN_FIXTURES=1 to regenerate)"
    assert FIXTURE.stat().st_size < 30_000
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    assert normal["reviewer_calls"] == 1 and recovery["reviewer_calls"] == 1
    assert recovery["retry_response_reused"] and recovery["retry_review_delta"] == 0, (first_response, retry)
    assert recovery["second_result_reused"] and recovery["second_result_review_delta"] == 0
    assert failed["mode"] == "FULL" and failed["reviewer_result"] is None
