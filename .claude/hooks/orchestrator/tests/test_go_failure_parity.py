import json, os, sys
from pathlib import Path
ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude/hooks/orchestrator"
FIXTURE = ORCH / "tests/fixtures/go_policy/failure.json"
sys.path.insert(0, str(ORCH))
import codex, ledger
CASES = ("sync_failure", "sync_interrupt", "reply_replay_failure", "missing_pending", "malformed_pending")
PENDING = {"project_key": "0123456789abcdef", "route": "operation", "model": "gpt-6-luna",
           "effort": "high", "agent": "codex", "phase": "operation", "scope": [],
           "reason": "continue", "reason_source": "explicit", "eta_minutes": 5,
           "previous_route": "normal", "new_route": "operation", "prior_thread_id": "prior-thread",
           "label": "P94 failure"}

def test_python_failure_oracle_fixture(tmp_path):
    actual = []
    for name in CASES:
        replay, invalid = name == "reply_replay_failure", name in {"missing_pending", "malformed_pending"}
        event = {"tool_name": "mcp__codex-main__codex-reply" if replay else "mcp__codex-main__codex",
                 "tool_use_id": name, "session_id": "p94-session", "error": "worker failed",
                 "is_interrupt": name == "sync_interrupt", "duration_ms": 1234}
        if replay: event.update(error_code="JOB_REPLAY_PREPARATION_FAILED", replay_thread_id="replayed-thread")
        pending = dict(PENDING, task_id="p94-" + name)
        root = tmp_path / name; root.mkdir()
        path = codex._pending_path(root, event); path.parent.mkdir(parents=True)
        if name == "malformed_pending": path.write_text("{", encoding="utf-8")
        elif not invalid: path.write_text(json.dumps(pending), encoding="utf-8")
        codex.post_tool_use_failure(event, state_root=root)
        evidence_path = root / "raw_tool_outputs" / f"{codex._call_key(event)}-failure.json"
        evidence = json.loads(evidence_path.read_text(encoding="utf-8")) if evidence_path.exists() else None
        saved = ledger.SessionLedger(root / "projects" / pending["project_key"]).load(pending["task_id"])
        session = saved.get("record") if saved.get("status") == ledger.OK else None
        if session: session["last_outcome"]["evidence_path"] = session["last_outcome"]["evidence_path"].replace(str(root), "__ROOT__")
        obs = root / "telemetry/events/by_task" / f"{pending['task_id']}.jsonl"
        observation = json.loads(obs.read_text(encoding="utf-8").splitlines()[-1]) if obs.exists() else None
        payload = {k: v for k, v in observation["payload"].items() if not k.endswith("_at")} if observation else None
        actual.append({"name": name, "event": event, "pending": pending,
                       "evidence": evidence, "session": session, "observation_payload": payload})
    if os.getenv("P94_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps({"cases": actual}, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert [c["name"] for c in fixture["cases"]] == list(CASES) and actual == fixture["cases"]
