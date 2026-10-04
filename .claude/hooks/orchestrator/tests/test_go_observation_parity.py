import json
import os
from pathlib import Path
import subprocess
import sys
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone

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


def test_python_observation_p50_spool_merge_oracle(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    cases = fixture["p50_cases"]
    merge_cases = [case for case in cases if case["mode"] == "merge"]
    state = tmp_path / "python-merge"
    root = telemetry._root(state)
    assert all(telemetry_schema.valid_event(case["event"]) for case in merge_cases)
    assert all(telemetry._encoded_event(case["event"]).decode("utf-8") == case["python_line"] for case in merge_cases)
    for case in merge_cases:
        telemetry._spool_event(root, case["event"])
    telemetry.emit("TELEMETRY_WARNING", state_root=state, session_id="p50-trigger", warning_code="MALFORMED_VERDICT")

    session_log = root / "events" / "2026-10" / "session_p50-session.jsonl"
    assert session_log.read_bytes() == "".join(case["python_line"] for case in merge_cases).encode("utf-8")
    spool = root / "events" / "spool"
    for case in merge_cases:
        event = case["event"]
        assert (spool / ".merged" / event["event_id"]).is_file()
        assert not (spool / f"{event['event_id']}.json").exists()
        index = root / "events" / "by_task" / f"{event['task_id']}.jsonl"
        assert index.read_bytes() == case["python_line"].encode("utf-8")

    for case in (item for item in cases if item["mode"] == "reject"):
        rejected_state = tmp_path / case["name"]
        rejected_root = telemetry._root(rejected_state)
        rejected_spool = rejected_root / "events" / "spool"
        rejected_spool.mkdir(parents=True, exist_ok=True)
        raw = case.get("raw")
        if raw is None:
            raw = json.dumps(case["event"], ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        (rejected_spool / case["filename"]).write_text(raw, encoding="utf-8")
        telemetry._merge_spool(rejected_root)
        assert not (rejected_spool / case["filename"]).exists(), case["name"]
        health = json.loads((rejected_root / "telemetry_health.json").read_text(encoding="utf-8"))
        assert health["last_error_code"] == case["health_code"], case["name"]


def test_go_observation_p50_cases():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestObservation.*P50", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)


def test_python_observation_p51_usage_oracle(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    for case in fixture["p51_cases"]:
        state = tmp_path / case["name"]
        snapshot_path = state / "telemetry" / "latest" / "claude_usage.json"
        captured_at = None
        if "snapshot" in case:
            snapshot = dict(case["snapshot"])
            captured_at = (datetime.now(timezone.utc).replace(microsecond=0)
                           - timedelta(seconds=snapshot.pop("age_seconds")))
            snapshot["captured_at"] = captured_at.isoformat().replace("+00:00", "Z")
            snapshot_path.parent.mkdir(parents=True, exist_ok=True)
            snapshot_path.write_text(json.dumps(snapshot), encoding="utf-8")
        elif "malformed" in case:
            snapshot_path.parent.mkdir(parents=True, exist_ok=True)
            snapshot_path.write_text(case["malformed"], encoding="utf-8")

        telemetry.emit(case["type"], task_id=case["task"], state_root=state,
                       **case["envelope"], **case["payload"])
        events = telemetry._root(state) / "events"
        path = next(events.glob("????-??/session_p51-session.jsonl"))
        event = json.loads(path.read_text(encoding="utf-8"))
        expected = dict(case["expected"])
        if "$CAPTURED_AT" in expected.values():
            expected["claude_usage_captured_at"] = captured_at.isoformat().replace("+00:00", "Z")
        assert event["payload"] == expected, case["name"]
        assert telemetry_schema.valid_event(event), case["name"]


def test_go_observation_p51_cases():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestObservation.*P51", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)


def test_python_observation_p52_aggregate_oracle(monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    monkeypatch.setattr(telemetry, "_policy_hashes", lambda: ({
        "skill_sha256": None, "config_sha256": None, "hook_sha256": None
    }, None))
    monkeypatch.setattr(telemetry, "emit", lambda *_a, **_k: None)
    for case in fixture["p52_cases"]:
        aggregate = telemetry._new_aggregate(case["task"], case["project_key"], case.get("existing"))
        if case.get("existing") is not None:
            aggregate.update(case["existing"])
            if isinstance(aggregate.get("usage"), dict):
                usage = telemetry._usage_defaults()
                usage.update(aggregate["usage"])
                aggregate["usage"] = usage
        decision_conflict = False
        for event in sorted(case["events"], key=lambda item: (str(item["ts_utc"]), str(item["event_id"]))):
            decision_conflict = telemetry._fold_event(aggregate, event) or decision_conflict
        canonical = lambda value: json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        assert canonical(aggregate) == canonical(case["expected"]), case["name"]
        assert decision_conflict is case["decision_conflict"], case["name"]


def test_go_observation_p52_aggregate_cases():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestObservation.*P52", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)
