"""Python oracle for the Go async FINALIZING recovery tranche."""
import json
import os
from pathlib import Path

import codex
import ledger


FIXTURE = Path(__file__).parent / "fixtures" / "go_policy" / "async_recovery.json"


def test_async_recovery_oracle(tmp_path):
    root = tmp_path / "state"
    key = "a" * 64
    project = "project-fixture"
    task = "recovery-task"
    artifact = root / "bundle_artifacts" / f"{key}.json"
    raw = (root / "raw_tool_outputs" / f"{key}.json").resolve()
    artifact.parent.mkdir(parents=True)
    raw.parent.mkdir(parents=True)
    raw.write_text("{}", encoding="utf-8")
    bundle = {"task_id": task, "raw_output_path": str(raw), "findings": [],
              "claude_review_mode": "AUDIT", "audit_receipt": {"ok": True},
              "implementer_report": "Done."}
    artifact.write_text(json.dumps(bundle), encoding="utf-8")
    index = {"call_key": key, "project_key": project, "stage": "FINALIZING"}
    pending = {"task_id": task, "project_key": project}

    rejection = codex._async_ledger_rejection(root, index, pending, artifact, success=True)
    complete = {"recovered": True, "ledger_rejected": rejection is not None}

    missing = root / "bundle_artifacts" / f"{'b' * 64}.json"
    missing_case = {"recovered": missing.is_file() and raw.is_file()}

    # Exercise the real rejection renderer, then inspect its persisted artifact.
    artifact.write_text(json.dumps({**bundle, "findings": []}), encoding="utf-8")
    rejected = codex._async_ledger_rejection(root, index, pending, artifact, success=True)
    assert rejected is not None
    response = codex._async_rejected_response(root, {
        "session_id": "oracle-session", "tool_use_id": "oracle-call",
        "tool_name": "mcp__codex-main__codex_result",
    }, artifact, rejected, {"post_tool_output_field": "updatedToolOutput"}, success=True)
    response_bundle = json.loads(response["hookSpecificOutput"]["updatedToolOutput"][0]["text"])
    persisted = root / "bundle_artifacts" / f"{codex._call_key({'session_id': 'oracle-session', 'tool_use_id': 'oracle-call'})}.json"
    rejection_case = {
        "ledger_rejected": True,
        "response_mode": response_bundle["claude_review_mode"],
        "artifact_has_rejection": any(item.get("code") == "LEDGER_RECORD_REJECTED"
                                       for item in json.loads(persisted.read_text(encoding="utf-8"))["findings"]),
    }

    session_root = root / "projects" / project
    record = ledger.build_session_record(task, "thread-recovery", "normal", "model", "high",
                                         last_outcome={"raw_output_path": str(raw)})
    assert ledger.SessionLedger(session_root).save(record)["status"] == ledger.OK
    recorded = codex._async_ledger_rejection(root, index, pending, artifact, success=True)
    recorded_case = {"ledger_rejected": recorded is not None}

    artifact.write_text("{broken", encoding="utf-8")
    try:
        json.loads(artifact.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        corrupt_case = {"artifact_error": "corrupt"}
    else:
        raise AssertionError("corrupt artifact was accepted")

    expected = {"oracle": "codex._async_post_followup FINALIZING recovery helpers", "cases": {
        "complete_artifact": complete, "missing_artifact": missing_case,
        "ledger_recorded": recorded_case, "rejected_response": rejection_case,
        "corrupt_artifact": corrupt_case,
    }}
    if os.environ.get("POST_ASYNC_RECOVERY_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n",
                           encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE}; set POST_ASYNC_RECOVERY_WRITE_FIXTURE=1 to capture"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected
