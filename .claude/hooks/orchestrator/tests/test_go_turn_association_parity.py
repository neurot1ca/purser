import json
from contextlib import contextmanager
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "turn_association.json"
sys.path.insert(0, str(ORCHESTRATOR))
import context_io_telemetry as context_io


def _journal_path(state, session_id):
    key = context_io._session_key(session_id)
    return context_io._paths(state, key)[0] if key else None


def test_p62_python_associate_turn_golden_fixture(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "context_io_telemetry.associate_turn"
    assert all("p62" in case["name"] for case in fixture["cases"])
    actual = []
    for case in fixture["cases"]:
        state = tmp_path / case["name"]
        session_id = case["session_id"]
        journal = _journal_path(state, session_id)
        if journal is not None and case["initial_journal"] is not None:
            journal.parent.mkdir(parents=True, exist_ok=True)
            initial = case["initial_journal"]
            if isinstance(initial, dict):
                initial = dict(initial)
                initial["opened_at"] = context_io.time.time()
                journal.write_text(json.dumps(initial), encoding="utf-8")
            else:
                journal.write_text(initial, encoding="utf-8")
        if journal is not None and case.get("contention"):
            context_io._paths(state, context_io._session_key(session_id))[2].parent.mkdir(parents=True, exist_ok=True)
            context_io._paths(state, context_io._session_key(session_id))[2].touch()
        if case.get("lock_contention"):
            @contextmanager
            def unavailable(_path):
                yield False

            monkeypatch.setattr(context_io, "_session_lock", unavailable)
        result = context_io.associate_turn(
            state, session_id=session_id, task_id=case["task_id"], project_key=case["project_key"]
        )
        observed = None
        if journal is not None and journal.exists():
            try:
                observed = json.loads(journal.read_text(encoding="utf-8"))
                if isinstance(observed, dict):
                    observed["opened_at"] = "$NOW"
            except json.JSONDecodeError:
                observed = journal.read_text(encoding="utf-8")
        actual.append({"name": case["name"], "return": result, "journal": observed})
        monkeypatch.undo()
    expected = [
        {"name": case["name"], "return": case["expected_return"], "journal": case["expected_journal"]}
        for case in fixture["cases"]
    ]
    assert actual == expected
