import json, os, sys
from pathlib import Path
from unittest.mock import patch
ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude/hooks/orchestrator"
FIXTURE = ORCH / "tests/fixtures/go_policy/failure.json"
sys.path.insert(0, str(ORCH))
import codex, ledger
CASES = ("sync_failure", "sync_interrupt", "reply_replay_failure", "missing_pending", "malformed_pending",
         "lease_release_failure", "pending_removal_failure", "async_job_record_removal", "not_enforced_note")
PENDING = {"project_key": "0123456789abcdef", "route": "operation", "model": "gpt-6-luna",
           "effort": "high", "agent": "codex", "phase": "operation", "scope": [],
           "reason": "continue", "reason_source": "explicit", "eta_minutes": 5,
           "previous_route": "normal", "new_route": "operation", "prior_thread_id": "prior-thread",
           "label": "P94 failure"}

def test_python_failure_oracle_fixture(tmp_path, monkeypatch):
    isolated_home = tmp_path / "home"
    monkeypatch.setenv("HOME", str(isolated_home))
    monkeypatch.setenv("USERPROFILE", str(isolated_home))
    monkeypatch.setenv("HOMEDRIVE", "")
    monkeypatch.setenv("HOMEPATH", "")
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex"))
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "claude-config"))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "xdg-config"))
    monkeypatch.setenv("APPDATA", str(tmp_path / "appdata"))
    monkeypatch.setenv("LOCALAPPDATA", str(tmp_path / "localappdata"))
    actual = []
    for name in CASES:
        replay, invalid = name == "reply_replay_failure", name in {"missing_pending", "malformed_pending"}
        start = name == "async_job_record_removal"
        bypass = name == "not_enforced_note"
        event = {"tool_name": "mcp__codex-main__codex_start" if start else
                 "mcp__codex-main__codex-reply" if replay else "mcp__codex-main__codex",
                 "tool_use_id": name, "session_id": "p94-session", "error": "worker failed",
                 "is_interrupt": name == "sync_interrupt", "duration_ms": 1234}
        if start:
            event["tool_input"] = {"call": "async-call-p95"}
        if bypass:
            event["tool_input"] = {"prompt": "bypass"}
        if replay: event.update(error_code="JOB_REPLAY_PREPARATION_FAILED", replay_thread_id="replayed-thread")
        pending = dict(PENDING, task_id="p94-" + name)
        if name == "lease_release_failure": pending["lease_owner"] = "p95-owner"
        if start:
            pending.update(async_call="async-call-p95", job_id="a" * 64)
        if bypass:
            pending["kind"] = "pretool_bypass"
        root = tmp_path / name; root.mkdir()
        monkeypatch.setenv("PURSER_STATE_DIR", str(root))
        path = codex._bypass_path(root, event) if bypass else codex._pending_path(root, event)
        path.parent.mkdir(parents=True)
        if name == "malformed_pending": path.write_text("{", encoding="utf-8")
        elif not invalid: path.write_text(json.dumps(pending), encoding="utf-8")
        if start:
            jobs = root / "async_jobs"; jobs.mkdir()
            (jobs / ("a" * 64 + ".json")).write_text("{}", encoding="utf-8")
        with patch.object(codex, "_release_scope_lease", side_effect=codex.WiringError("LEASE_RELEASE_FAILED")) if name == "lease_release_failure" else patch.object(codex, "_release_scope_lease", wraps=codex._release_scope_lease), \
             patch.object(codex, "_remove_pending", side_effect=PermissionError("pending removal denied")) if name == "pending_removal_failure" else patch.object(codex, "_remove_pending", wraps=codex._remove_pending), \
             patch.object(codex, "_cancel_start_job", return_value=False) if start else patch.object(codex, "_cancel_start_job", wraps=codex._cancel_start_job), \
             patch.object(codex, "_start_job_terminal_or_absent", return_value=True) if start else patch.object(codex, "_start_job_terminal_or_absent", wraps=codex._start_job_terminal_or_absent):
            response = codex.post_tool_use_failure(event, state_root=root)
        output = response.get("hookSpecificOutput", {}).get("additionalContext", "").replace(str(root).replace("\\", "\\\\"), "__ROOT__").replace(str(root), "__ROOT__")
        output = output.replace("\\", "/") if "__ROOT__" in output else output
        evidence_path = root / "raw_tool_outputs" / f"{codex._call_key(event)}-failure.json"
        evidence = json.loads(evidence_path.read_text(encoding="utf-8")) if evidence_path.exists() else None
        saved = ledger.SessionLedger(root / "projects" / pending["project_key"]).load(pending["task_id"])
        session = saved.get("record") if saved.get("status") == ledger.OK else None
        if session:
            evidence_path = session["last_outcome"]["evidence_path"]
            evidence_path = evidence_path.replace(str(root).replace("\\", "\\\\"), "__ROOT__").replace(str(root), "__ROOT__")
            session["last_outcome"]["evidence_path"] = evidence_path.replace("\\", "/") if "__ROOT__" in evidence_path else evidence_path
        obs = root / "telemetry/events/by_task" / f"{pending['task_id']}.jsonl"
        observation = json.loads(obs.read_text(encoding="utf-8").splitlines()[-1]) if obs.exists() else None
        payload = {k: v for k, v in observation["payload"].items() if not k.endswith("_at")} if observation else None
        job_path = root / "async_jobs" / ("a" * 64 + ".json")
        actual.append({"name": name, "event": event, "pending": pending,
                       "evidence": evidence, "session": session, "observation_payload": payload,
                       "output": output, "pending_exists": path.exists(),
                       "async_job_exists": job_path.exists() if start else None})
    if os.getenv("P94_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps({"cases": actual}, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8", newline="\n")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert [c["name"] for c in fixture["cases"]] == list(CASES) and actual == fixture["cases"]
