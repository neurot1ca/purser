import json
import shutil
import sys
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from codex_quota import read_latest_quota, read_quota


THREAD = "01234567-89ab-cdef-0123-456789abcdef"


@pytest.fixture
def quota_home():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "quota" / uuid.uuid4().hex
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        shutil.rmtree(root)


def rollout(tmp_path, lines):
    folder = tmp_path / "sessions" / "2026" / "09" / "23"
    folder.mkdir(parents=True)
    (folder / f"rollout-2026-09-23T12-00-00-{THREAD}.jsonl").write_text(
        "\n".join(lines) + "\n", encoding="utf-8"
    )


def record(used, *, secondary=True, primary_reset=1790173742):
    limits = {"primary": {"used_percent": used, "window_minutes": 300, "resets_at": primary_reset}}
    if secondary:
        limits["secondary"] = {"used_percent": 38, "window_minutes": 10080, "resets_at": 1790611663}
    return json.dumps({"type": "event_msg", "payload": {"type": "token_count", "rate_limits": limits}})


def test_quota_newest_rate_limits_wins(quota_home):
    rollout(quota_home, [record(11), record(24)])
    assert read_quota(THREAD, codex_home=quota_home) == {
        "5h_used_pct": 24,
        "5h_resets_at": "2026-09-23T14:29:02Z",
        "7d_used_pct": 38,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }


def test_quota_preserves_rate_limit_snapshot_timestamp(quota_home):
    event = json.loads(record(24))
    event["timestamp"] = "2026-09-23T12:00:00Z"
    rollout(quota_home, [json.dumps(event)])

    assert read_quota(THREAD, codex_home=quota_home)["snapshot_at"] == event["timestamp"]


def test_quota_weekly_window_without_primary_omits_5h(quota_home):
    limits = {"secondary": {"used_percent": 38, "window_minutes": 10080, "resets_at": 1790611663}}
    event = json.dumps({"type": "event_msg", "payload": {"type": "token_count", "rate_limits": limits}})
    rollout(quota_home, [event])

    assert read_quota(THREAD, codex_home=quota_home) == {
        "7d_used_pct": 38,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }


def test_quota_no_windows_returns_none(quota_home):
    event = json.dumps({"type": "event_msg", "payload": {"type": "token_count", "rate_limits": {}}})
    rollout(quota_home, [event])

    assert read_quota(THREAD, codex_home=quota_home) is None


def test_quota_missing_file_returns_none(quota_home):
    assert read_quota(THREAD, codex_home=quota_home) is None


def test_quota_skips_malformed_lines(quota_home):
    rollout(quota_home, [record(11, secondary=False), "{invalid"])
    assert read_quota(THREAD, codex_home=quota_home)["5h_used_pct"] == 11


def test_quota_without_rate_limits_returns_none(quota_home):
    rollout(quota_home, [json.dumps({"type": "event_msg", "payload": {"type": "token_count"}})])
    assert read_quota(THREAD, codex_home=quota_home) is None


def _dated_rollout(home, day, name, content, mtime):
    path = home / "sessions" / day.strftime("%Y/%m/%d") / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content + "\n", encoding="utf-8")
    import os
    os.utime(path, (mtime, mtime))
    return path


def test_latest_quota_chooses_most_recent_mtime_across_today_and_yesterday(quota_home):
    now = datetime(2026, 9, 24, 12, tzinfo=timezone.utc)
    older = _dated_rollout(quota_home, now.date(), "rollout-today.jsonl", record(31, primary_reset=1790263742), 100)
    newer = _dated_rollout(quota_home, now.date() - timedelta(days=1), "rollout-yesterday.jsonl", record(72, primary_reset=1790263742), 200)
    assert older.exists()
    assert read_latest_quota(codex_home=quota_home, now=now) == {
        "5h_used_pct": 72,
        "5h_resets_at": "2026-09-24T15:29:02Z",
        "7d_used_pct": 38,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }


def test_latest_quota_drops_expired_windows(quota_home):
    now = datetime(2026, 9, 24, 12, tzinfo=timezone.utc)
    _dated_rollout(quota_home, now.date(), "rollout-expired.jsonl", record(92), 100)
    assert read_latest_quota(codex_home=quota_home, now=now) == {
        "7d_used_pct": 38,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }


def test_latest_quota_missing_sessions_returns_none(quota_home):
    assert read_latest_quota(codex_home=quota_home, now=datetime.now(timezone.utc)) is None
