import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "session_lookup.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger


def _python_lookup(tmp_path, case):
    root = tmp_path / case["name"]
    sessions = root / "sessions"
    sessions.mkdir(parents=True)
    for name, envelope in case["files"].items():
        (sessions / name).write_text(json.dumps(envelope), encoding="utf-8")
    return ledger.SessionLedger(root).find_by_thread_id(case["thread_id"])


def _guard_rejection(case):
    if case["name"] not in {"invalid-record-ignored", "checksum-failure-ignored"}:
        return
    assert len(case["files"]) == 1
    envelope = next(iter(case["files"].values()))
    payload = envelope["payload"]
    expected = hashlib.sha256(ledger._canonical_json(payload)).hexdigest()
    assert payload["live_thread"]["thread_id"] == case["thread_id"]
    if case["name"] == "invalid-record-ignored":
        assert envelope["checksum"] == expected
        assert not ledger._valid_session_record(payload, payload["task_id"])
    else:
        assert ledger._valid_session_record(payload, payload["task_id"])
        assert envelope["checksum"] != expected


def test_python_oracle_matches_session_lookup_fixture(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python ledger.build_session_record and SessionLedger.find_by_thread_id"
    for case in fixture["construction"]:
        inputs = case["input"]
        actual = ledger.build_session_record(
            inputs["task_id"], inputs["thread_id"], inputs["route"], inputs["model"], inputs["effort"],
            **inputs["options"],
        )
        assert actual == case["output"], inputs["task_id"]
    for case in fixture["lookup"]:
        _guard_rejection(case)
        assert _python_lookup(tmp_path, case) == case["output"], case["name"]


def test_go_session_lookup_fixture():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    env = {
        **os.environ,
        "GOCACHE": str(scratch / "go-cache"),
        "TMP": str(scratch),
        "TEMP": str(scratch),
        "TMPDIR": str(scratch),
        "PYTHONDONTWRITEBYTECODE": "1",
    }
    (scratch / "go-cache").mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["go", "test", "-run", "^TestSessionLookupParityFixture$", "-count=1", "./internal/ledger/..."],
        cwd=ROOT / "go", env=env, capture_output=True, text=True, check=False,
    )
    assert result.returncode == 0, result.stdout + result.stderr
