from __future__ import annotations

import re
import sys
import threading
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import state_retention as retention


def _set_mtime(path: Path, stamp: float) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(b"x")
    import os
    os.utime(path, (stamp, stamp))
    return path


def _written_state_names() -> set[str]:
    package = Path(__file__).resolve().parents[1]
    names: set[str] = set()
    py_patterns = (
        r"(?:state_root|runtime_root|default_state_root\(\))\s*/\s*['\"]([^'\"]+)['\"]",
    )
    for source in package.glob("*.py"):
        text = source.read_text(encoding="utf-8", errors="ignore")
        for pattern in py_patterns:
            names.update(match.group(1) for match in re.finditer(pattern, text))
        if re.search(r"\broot\s*=\s*Path\(state_root\)", text):
            names.update(re.findall(r"\broot\s*/\s*['\"]([^'\"]+)['\"]", text))
    go_root = package.parents[2] / "go" / "internal"
    pattern = re.compile(r"filepath\.Join\(\s*stateRoot\s*,\s*['\"]([^'\"]+)['\"]")
    for source in go_root.rglob("*.go"):
        if source.name.endswith("_test.go"):
            continue
        text = source.read_text(encoding="utf-8", errors="ignore")
        names.update(match.group(1) for match in pattern.finditer(text))
    # These paths are repository inputs or descendants, not state-root entries.
    return {name for name in names if "/" not in name and "\\" not in name
            and name not in {"duration_accuracy.json"}
            and name not in {".claude", ".claude-plugin", ".tmp", "CHANGELOG.md", "README.md", "MEASURE.md"}}


def _unregistered(written: set[str], registry) -> set[str]:
    return written - set(registry)


def _assert_registry_complete(written: set[str], registry) -> None:
    missing = _unregistered(written, registry)
    assert not missing, sorted(missing)


def test_state_root_writers_are_registered():
    written = _written_state_names()
    _assert_registry_complete(written, retention.REGISTRY)
    # Removing an entry from a temporary registry copy makes the guard fail.
    omitted = next(iter(written))
    incomplete = set(retention.REGISTRY) - {omitted}
    assert _unregistered(written, incomplete) == {omitted}
    with pytest.raises(AssertionError, match=omitted):
        _assert_registry_complete(written, incomplete)


def test_age_then_oldest_count_and_byte_pruning(tmp_path):
    root = tmp_path / "files"
    now = 2_000_000
    old = _set_mtime(root / "expired", now - 90_000)
    oldest = _set_mtime(root / "oldest", now - 3000)
    middle = _set_mtime(root / "middle", now - 2000)
    newest = _set_mtime(root / "newest", now - 1000)
    retention._prune(root, retention.Policy(max_age_days=1, max_files=2, max_bytes=1,
                                           delete_patterns=("*",)), now)
    assert not old.exists()
    assert not oldest.exists()
    assert not middle.exists()
    assert newest.exists()


def test_only_allowlisted_closed_patterns_are_deleted(tmp_path):
    root = tmp_path / "files"
    generated = _set_mtime(root / "closed.cache", 1000)
    durable = _set_mtime(root / "valuable.json", 1000)
    policy = retention.Policy(max_age_days=0, max_bytes=0, delete_patterns=("*.cache",))
    retention._prune(root, policy, 2000)
    assert not generated.exists()
    assert durable.exists()


def test_model_catalog_cache_is_the_only_production_delete_allowlist(tmp_path):
    policy = retention.REGISTRY["model-catalog"]
    assert policy.delete_patterns == ("catalog.json",)
    expired_cache = _set_mtime(tmp_path / "model-catalog" / "catalog.json", 1000)
    retention._prune(tmp_path / "model-catalog", policy, 1000 + 31 * 86400)
    assert not expired_cache.exists()


def test_recent_files_and_lock_files_are_protected(tmp_path):
    root = tmp_path / "files"
    recent = _set_mtime(root / "recent", 1000 - 30)
    lock = _set_mtime(root / "held.lock", 1000 - 1000)
    retention._prune(root, retention.Policy(max_age_days=0, max_files=0, max_bytes=0,
                                           delete_patterns=("*",)), 1000)
    assert recent.exists()
    assert lock.exists()


def test_append_only_log_is_not_rotated_without_safe_pattern(tmp_path):
    path = tmp_path / "diagnostics.jsonl"
    path.write_bytes(b"old\n" + b"a" * 40 + b"\nnewest\n")
    import os
    os.utime(path, (1000, 1000))
    original = path.read_bytes()
    retention._prune(path, retention.Policy(max_bytes=12), 2000)
    assert path.read_bytes() == original


def test_unknown_directory_uses_default_and_warns(tmp_path, monkeypatch):
    root = tmp_path / "state"
    unknown = root / "new-state"
    stale = _set_mtime(unknown / "stale", 1000)
    calls = []
    monkeypatch.setattr(retention.telemetry, "record_health", lambda *a, **kw: calls.append((a, kw)))
    retention.prune_state(root, now=1000 + 31 * 86400)
    assert stale.exists()
    assert calls
    assert calls[0][0][0] == "DEGRADED"


def test_telemetry_is_owner_managed(tmp_path, monkeypatch):
    event = _set_mtime(tmp_path / "telemetry" / "events" / "2026-01" / "session_x.jsonl", 1000)
    retention.prune_state(tmp_path, now=1000 + 100 * 86400)
    assert event.exists()


def test_projects_are_count_and_warn_only(tmp_path, monkeypatch):
    profile = _set_mtime(tmp_path / "projects" / "0123456789abcdef" / "profile.toml", 1000)
    calls = []
    monkeypatch.setattr(retention.telemetry, "record_health", lambda *a, **kw: calls.append((a, kw)))
    policies = dict(retention.REGISTRY)
    policies["projects"] = retention.Policy(max_files=0, max_bytes=0)
    monkeypatch.setattr(retention, "REGISTRY", policies)
    retention.prune_state(tmp_path, now=1000 + 86400)
    assert profile.exists()
    assert calls


def test_ceiling_walk_runs_in_background_at_most_daily(tmp_path, monkeypatch):
    owner_tree = tmp_path / "preparations"
    owner_tree.mkdir()
    for index in range(5000):
        (owner_tree / f"generation-{index}.bin").touch()
    calls = []
    scan_seconds = []
    original = retention._root_size
    def measured(root):
        started = time.perf_counter()
        calls.append(threading.current_thread().name)
        result = original(root)
        scan_seconds.append(time.perf_counter() - started)
        return result
    monkeypatch.setattr(retention, "_root_size", measured)
    monkeypatch.setattr(retention, "STATE_ROOT_MAX_BYTES", 10**12)
    started = time.perf_counter()
    retention.prune_state(tmp_path)
    synchronous_seconds = time.perf_counter() - started
    assert not calls
    def background_scan():
        retention.prune_state(tmp_path, check_ceiling=True)
        retention.prune_state(tmp_path, check_ceiling=True)
    worker = threading.Thread(target=background_scan, name="mcp-recovery-scan")
    worker.start()
    worker.join(timeout=5)
    assert not worker.is_alive()
    stamp = tmp_path / retention.CEILING_STAMP
    assert stamp.exists()
    assert len(calls) == 1
    assert calls[0] == "mcp-recovery-scan"
    assert synchronous_seconds < 1
    print(f"retention files=5000 sync_ms={synchronous_seconds * 1000:.2f} daily_scan_ms={scan_seconds[0] * 1000:.2f}")


def test_root_ceiling_warns(tmp_path, monkeypatch):
    calls = []
    monkeypatch.setattr(retention.telemetry, "record_health", lambda *a, **kw: calls.append((a, kw)))
    monkeypatch.setattr(retention, "_root_size", lambda _root: 101)
    monkeypatch.setattr(retention, "STATE_ROOT_MAX_BYTES", 100)
    retention._check_ceiling(tmp_path, time.time())
    assert calls


def test_owner_managed_entries_are_untouched(tmp_path):
    for name in ("preparations", "wt", "marketplace", "codex_exec_mcp", "async_jobs", "pending"):
        _set_mtime(tmp_path / name / "old", 1000)
    retention.prune_state(tmp_path, now=1000 + 100 * 86400)
    assert all((tmp_path / name / "old").exists() for name in
               ("preparations", "wt", "marketplace", "codex_exec_mcp", "async_jobs", "pending"))
