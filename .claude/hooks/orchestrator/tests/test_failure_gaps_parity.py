"""Python state oracle for the isolated Go PostToolUseFailure gap tranche."""
import json
import os
from pathlib import Path
import sys
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import telemetry
import codex

FIXTURE = Path(__file__).resolve().parents[1] / "tests/fixtures/go_policy/failure_gaps.json"


def _run(root: Path, name: str, monkeypatch):
    root.mkdir()
    event = {"tool_name": "mcp__codex-main__codex", "tool_use_id": name,
             "session_id": "failure-gaps", "hook_event_name": "PostToolUseFailure",
             "error": "worker failed", "is_interrupt": False, "duration_ms": 31}
    pending = {"task_id": "task-" + name, "project_key": "0123456789abcdef",
               "route": "normal", "model": "model-a", "effort": "high", "agent": "codex"}
    if name == "undelivered_marker":
        path = codex._pending_path(root, event)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(pending), encoding="utf-8")
        marker = root / "pending" / f"undelivered-{codex._call_key(event)}.json"
        marker.write_text(json.dumps({"reason": "adapter did not deliver"}), encoding="utf-8")
        observed = []
        with patch.object(codex, "_observe", side_effect=lambda *args, **kwargs: observed.append((args, kwargs))):
            codex.post_tool_use_failure(event, state_root=root)
        raw = json.loads((root / "raw_tool_outputs" / f"{codex._call_key(event)}-failure.json").read_text(encoding="utf-8"))
        violation = any(args[1] == "ROUTING_PROTOCOL_VIOLATION" for args, _ in observed)
        return {"kind": raw["pending_call"]["kind"], "reason": raw["pending_call"]["reason"],
                "marker_exists": marker.exists(), "protocol_violation": violation}
    if name == "mechanical_negative":
        pending.update(mechanical_opencode=True, agent="opencode")
        path = codex._pending_path(root, event)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(pending), encoding="utf-8")
        inventory = root / "opencode/candidates.json"
        inventory.parent.mkdir(parents=True)
        inventory.write_text(json.dumps({"candidates": [{"id": "model-a", "status": "available",
                                                            "mechanical_samples": [True]}]}), encoding="utf-8")
        codex.post_tool_use_failure(event, state_root=root)
        return {"samples": json.loads(inventory.read_text(encoding="utf-8"))["candidates"][0]["mechanical_samples"]}
    if name == "internal_diagnostic":
        path = codex._pending_path(root, event)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(pending), encoding="utf-8")
        with patch.object(codex, "_persist_raw_output", side_effect=PermissionError("fixture denied")):
            codex.post_tool_use_failure(event, state_root=root)
        record = json.loads((root / "diagnostics.jsonl").read_text(encoding="utf-8").splitlines()[-1])
        bundle = codex._base_bundle(record["error"])
        return {"phase": record["phase"], "finding": bundle["findings"][0]["code"],
                "review_mode": bundle["claude_review_mode"]}
    path = codex._pending_path(root, event)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(pending), encoding="utf-8")
    if name == "observation_failure_tolerated":
        monkeypatch.setattr(telemetry, "emit", lambda *a, **kw: (_ for _ in ()).throw(OSError("fixture telemetry failure")))
    response = codex.post_tool_use_failure(event, state_root=root)
    if name == "task_projection":
        projection = root / "projects/0123456789abcdef/telemetry/tasks" / f"task-{name}.json"
        return {"projected": projection.exists()}
    return {"returned": response["hookSpecificOutput"]["hookEventName"] == "PostToolUseFailure"}


def test_python_failure_gaps_fixture(tmp_path, monkeypatch):
    names = ("undelivered_marker", "mechanical_negative", "internal_diagnostic",
             "task_projection", "observation_failure_tolerated")
    actual = {name: _run(tmp_path / name, name, monkeypatch) for name in names}
    if os.environ.get("FAILURE_GAPS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == actual
