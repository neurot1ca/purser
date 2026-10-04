import json
import os
from pathlib import Path
import subprocess
import sys

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
