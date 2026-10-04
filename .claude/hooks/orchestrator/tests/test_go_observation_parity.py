import json
import os
from pathlib import Path
import subprocess
import sys
from contextlib import contextmanager

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "observations.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex
import telemetry
import telemetry_schema


def test_python_observation_p48_oracle(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python telemetry_schema and telemetry.record_health"
    for case in fixture["cases"]:
        if "p48" not in case["name"]:
            continue
        try:
            event = telemetry_schema.build_event(case["type"], task_id=case["task"], payload=case["payload"], **case["envelope"])
        except (TypeError, ValueError):
            assert case["build"] is False, case["name"]
        else:
            assert case["build"] is True, case["name"]
            assert event["payload"] == case["clean"], case["name"]
            for key, expected in case.get("expected", {}).items():
                assert event[key] == expected, (case["name"], key)
            assert telemetry_schema.valid_event(event), case["name"]
    health_root = tmp_path / "state"
    telemetry.record_health("DEGRADED", error_code="CLAUDE_TRANSCRIPT_PARSER_FAILED", state_root=health_root)
    health = json.loads((health_root / "telemetry" / "telemetry_health.json").read_text(encoding="utf-8"))
    assert (health["status"], health["failure_count"], health["last_error_code"]) == (
        "DEGRADED", 1, "CLAUDE_TRANSCRIPT_PARSER_FAILED"
    )

    monkeypatch.setattr(telemetry, "emit", lambda *_a, **_k: (_ for _ in ()).throw(OSError("writer")))
    monkeypatch.setattr(telemetry, "record_health", lambda *_a, **_k: (_ for _ in ()).throw(OSError("health")))
    result = codex._observed_denial({}, "UNKNOWN_KEY", "policy denial", state_root=health_root)
    assert result == codex._deny("UNKNOWN_KEY", "policy denial")


def test_go_observation_p48_cases():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestObservation.*P48", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)


def test_python_observation_p49_emit_oracle(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    cases = fixture["p49_cases"]
    original_try_lock = telemetry._try_lock
    @contextmanager
    def contended(_root):
        yield False
    for case in cases:
        state = tmp_path / case["name"]
        if case["mode"] == "spool":
            with monkeypatch.context() as scoped:
                scoped.setattr(telemetry, "_try_lock", contended)
                telemetry.emit(case["type"], task_id=case["task"], state_root=state,
                               **case["envelope"], **case["payload"])
        else:
            telemetry.emit(case["type"], task_id=case["task"], state_root=state,
                           **case["envelope"], **case["payload"])
        events = state / "telemetry" / "events"
        if case["mode"] == "append":
            raw = next(events.glob("????-??/session_*.jsonl"))
            event = json.loads(raw.read_text(encoding="utf-8"))
            indexed = json.loads((events / "by_task" / f"{case['task']}.jsonl").read_text(encoding="utf-8"))
            assert event == indexed and telemetry_schema.valid_event(event), case["name"]
            assert event["payload"] == case["payload"] and event["task_id"] == case["task"]
        else:
            spooled = json.loads(next((events / "spool").glob("*.json")).read_text(encoding="utf-8"))
            assert telemetry_schema.valid_event(spooled), case["name"]
            assert spooled["payload"] == case["payload"] and spooled["task_id"] == case["task"]
            assert not (events / "by_task" / f"{case['task']}.jsonl").exists()

    state = tmp_path / "forced-contention"
    monkeypatch.setattr(telemetry, "_try_lock", contended)
    case = cases[1]
    telemetry.emit(case["type"], task_id=case["task"], state_root=state,
                   **case["envelope"], **case["payload"])
    events = state / "telemetry" / "events"
    spool = next((events / "spool").glob("*.json"))
    assert not (events / "by_task" / f"{case['task']}.jsonl").exists()
    assert json.loads(spool.read_text(encoding="utf-8"))["task_id"] == case["task"]

    failing = tmp_path / "writer-failure"
    monkeypatch.setattr(telemetry, "_try_lock", original_try_lock)
    monkeypatch.setattr(telemetry, "_append_event", lambda *_a, **_k: (_ for _ in ()).throw(OSError("append")))
    monkeypatch.setattr(telemetry, "_record_failure", lambda root, code: Path(root, "failure.marker").write_text(code))
    telemetry.emit(case["type"], task_id=case["task"], state_root=failing,
                   **case["envelope"], **case["payload"])
    assert (failing / "telemetry" / "failure.marker").read_text() == "TELEMETRY_EMIT_FAILED"
    monkeypatch.setattr(telemetry, "emit", lambda *_a, **_k: (_ for _ in ()).throw(OSError("writer")))
    assert codex._observed_denial({}, "UNKNOWN_KEY", "policy denial", state_root=failing) == codex._deny("UNKNOWN_KEY", "policy denial")


def test_go_observation_p49_cases():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestObservation.*P49", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)
