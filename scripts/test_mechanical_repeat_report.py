import hashlib
import importlib.util
import json
from pathlib import Path


spec = importlib.util.spec_from_file_location(
    "mechanical_repeat_report", Path(__file__).with_name("mechanical_repeat_report.py")
)
reporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reporter)


def write_events(root, task_id, entries):
    path = root / "projects" / "0123456789abcdef" / "telemetry" / "events" / "by_task" / f"{task_id}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(item) + "\n" for item in entries), encoding="utf-8")


def event(task, thread, fingerprint, classification="rename", ts="2026-09-28T00:00:00Z"):
    return {
        "event_type": "MECHANICAL_EDIT_OBSERVED",
        "ts_utc": ts,
        "task_id": task,
        "project_key": "0123456789abcdef",
        "payload": {
            "thread_id": thread,
            "classification": classification,
            "fingerprint": fingerprint,
            "changed_file_count": 1,
            "hunk_count": 1,
        },
    }


def test_report_counts_repeated_identifier_rename_and_keeps_near_matches_separate(tmp_path):
    root = tmp_path / "state"
    fingerprint = hashlib.sha256(b"rename:old_name->new_name:.py").hexdigest()
    write_events(root, "task-one", [event("task-one", "thread-one", fingerprint)])
    write_events(root, "task-two", [event("task-two", "thread-two", fingerprint)])
    write_events(root, "task-three", [event("task-three", "thread-three", None, "unclassified")])
    write_events(root, "task-four", [event("task-four", "thread-four", None, "unclassified")])

    result = reporter.report(root, reporter.timestamp("2026-09-28T00:00:00Z"))

    assert result["usable_diff_jobs"] == 4
    assert result["classified_jobs"] == 2
    assert result["unclassified_jobs"] == 2
    repeated, = result["repeated_fingerprints"]
    assert repeated["fingerprint"] == fingerprint
    assert repeated["count"] == 2
    assert repeated["task_ids"] == ["task-one", "task-two"]


def test_report_caps_repeated_fingerprints_and_task_ids(tmp_path):
    root = tmp_path / "state"
    for group in range(6):
        fingerprint = hashlib.sha256(f"rename:old->{group}:py".encode()).hexdigest()
        count = 6 if group == 0 else 2
        for index in range(count):
            task_id = f"task-{group}-{index}"
            write_events(root, task_id, [event(task_id, f"thread-{task_id}", fingerprint)])

    result = reporter.report(root, reporter.timestamp("2026-09-28T00:00:00Z"))

    assert len(result["repeated_fingerprints"]) == 5
    assert all(len(item["task_ids"]) <= 5 for item in result["repeated_fingerprints"])
    assert any(item["count"] == 6 for item in result["repeated_fingerprints"])


def test_report_cli_writes_markdown_and_json(tmp_path, monkeypatch):
    root = tmp_path / "state"
    monkeypatch.setenv("PURSER_STATE_DIR", str(root))
    output = tmp_path / "report.md"

    reporter.main(["--since", "2026-09-28T00:00:00Z", "--out", str(output), "--json"])

    assert "Jobs with a usable review diff: 0" in output.read_text(encoding="utf-8")
    assert json.loads(output.with_suffix(".json").read_text(encoding="utf-8"))["usable_diff_jobs"] == 0
