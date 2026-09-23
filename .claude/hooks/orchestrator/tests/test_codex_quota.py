import json
import shutil
import sys
import uuid
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from codex_quota import read_quota


THREAD = "01234567-89ab-cdef-0123-456789abcdef"


@pytest.fixture
def quota_home():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "quota" / uuid.uuid4().hex
    root.mkdir(parents=True)
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


def record(used, *, secondary=True):
    limits = {"primary": {"used_percent": used, "window_minutes": 300, "resets_at": 1790173742}}
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


def test_quota_missing_file_returns_none(quota_home):
    assert read_quota(THREAD, codex_home=quota_home) is None


def test_quota_skips_malformed_lines(quota_home):
    rollout(quota_home, [record(11, secondary=False), "{invalid"])
    assert read_quota(THREAD, codex_home=quota_home)["5h_used_pct"] == 11


def test_quota_without_rate_limits_returns_none(quota_home):
    rollout(quota_home, [json.dumps({"type": "event_msg", "payload": {"type": "token_count"}})])
    assert read_quota(THREAD, codex_home=quota_home) is None
