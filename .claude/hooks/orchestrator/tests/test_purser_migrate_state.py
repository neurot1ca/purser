from pathlib import Path

import purser_migrate_state as migration


def _snapshot(root: Path):
    return {
        path.relative_to(root).as_posix(): path.read_bytes()
        for path in root.rglob("*")
        if path.is_file()
    }


def test_migration_copies_nested_files_skips_existing_and_writes_marker(tmp_path):
    legacy = tmp_path / "legacy"
    (legacy / "sessions" / "nested").mkdir(parents=True)
    (legacy / "sessions" / "nested" / "session.json").write_text("legacy", encoding="utf-8")
    (legacy / "telemetry.jsonl").write_text("events", encoding="utf-8")
    before = _snapshot(legacy)
    destination = tmp_path / "new-state"
    (destination / "sessions" / "nested").mkdir(parents=True)
    existing = destination / "sessions" / "nested" / "session.json"
    existing.write_text("keep this", encoding="utf-8")

    result = migration.migrate_state(legacy, destination)

    assert result["errors"] == []
    assert result["copied"] == 2
    assert result["skipped_existing"] == 1
    assert existing.read_text(encoding="utf-8") == "keep this"
    assert (destination / "telemetry.jsonl").read_text(encoding="utf-8") == "events"
    marker = (destination / "MIGRATED_FROM.txt").read_text(encoding="utf-8")
    assert f"legacy_path={legacy.resolve()}" in marker
    assert "migrated_at_utc=" in marker
    assert _snapshot(legacy) == before


def test_dry_run_reports_plan_without_writing(tmp_path):
    legacy = tmp_path / "legacy"
    (legacy / "nested").mkdir(parents=True)
    (legacy / "nested" / "file.txt").write_text("content", encoding="utf-8")
    destination = tmp_path / "new-state"

    result = migration.migrate_state(legacy, destination, dry_run=True)

    assert result["errors"] == []
    assert result["copied"] == 2
    assert result["skipped_existing"] == 0
    assert not destination.exists()
    assert _snapshot(legacy) == {"nested/file.txt": b"content"}


def test_destination_inside_legacy_is_rejected_without_changes(tmp_path):
    legacy = tmp_path / "legacy"
    legacy.mkdir()
    (legacy / "state.json").write_text("keep", encoding="utf-8")

    result = migration.migrate_state(legacy, legacy / "purser")

    assert result["errors"]
    assert _snapshot(legacy) == {"state.json": b"keep"}
