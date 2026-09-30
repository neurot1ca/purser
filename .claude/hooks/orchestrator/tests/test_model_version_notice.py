import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1]))
import model_version_notice


def test_detects_higher_version_with_same_model_stem():
    assert model_version_notice.newer_version(
        "gpt-6-luna", ["gpt-6.1-luna", "gpt-5-luna", "gpt-6.1-sol"]
    ) == "gpt-6.1-luna"


def test_ignores_other_family_older_and_unparsable_names():
    assert model_version_notice.newer_version(
        "gpt-6-luna", ["gpt-6.1-sol", "gpt-5-luna", "latest-luna", "gpt-luna"]
    ) is None


def test_declined_version_is_persisted(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path)
    model_version_notice.decide("gpt-6.1-luna", decline=True)
    assert model_version_notice.read_state()["declined"] == ["gpt-6.1-luna"]


def test_shown_version_is_persisted_and_clears_pending(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path)
    model_version_notice.write_state({"checked_on": None, "asked": [], "declined": [],
                                      "pending": {"current": "gpt-6-luna", "new": "gpt-6.1-luna"}})
    model_version_notice.mark_shown("gpt-6.1-luna")
    state = model_version_notice.read_state()
    assert state["asked"] == ["gpt-6.1-luna"]
    assert state["pending"] is None


def test_daily_check_marker_uses_one_date(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path)
    today = datetime(2026, 9, 30, tzinfo=timezone.utc)
    assert model_version_notice.due_today(now=today)
    model_version_notice.record_check(now=today)
    assert not model_version_notice.due_today(now=today)
    assert model_version_notice.due_today(now=datetime(2026, 10, 1, tzinfo=timezone.utc))
