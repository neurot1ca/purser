import json
import os
from pathlib import Path
import shutil
import sys
import stat
import uuid

import pytest


PACKAGE_ROOT = Path(__file__).parents[1]
sys.path.insert(0, str(PACKAGE_ROOT))

import obsidian_projection as projection
import telemetry
import telemetry_schema as schema


@pytest.fixture()
def tmp_path():
    root = Path(__file__).parent / ".obsidian-projection-test-tmp" / uuid.uuid4().hex
    root.mkdir(parents=True)
    try:
        yield root
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)

        shutil.rmtree(root, onerror=make_writable)
        try:
            root.parent.rmdir()
        except OSError:
            pass


def _aggregate(task_id="task-1", **changes):
    value = {
        "task_id": task_id,
        "repo": "repo",
        "branch": "main",
        "status": "VERIFIED",
        "started_at": "2026-08-30T08:00:00Z",
        "ended_at": "2026-08-30T08:00:01Z",
        "duration_ms": 100,
        "phase": "implementation",
        "route": "subtle",
        "model": "gpt-5.6-luna",
        "effort": "xhigh",
        "fresh_threads": 1,
        "reply_calls": 0,
        "routing_enforced": True,
        "validation": "PASS",
        "thread_id": "thread-1",
        "reviewer1_provider": "agy",
        "reviewer1_model_requested": "gemini-3.7-flash-high",
        "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
        "reviewer1_skipped_reason": None,
        "served_model_attestation": "UNAVAILABLE",
        "claude_review_mode": "FULL",
        "changed_paths_count": 1,
        "routing_protocol_violations": 0,
        "thread_continuity_violations": 0,
        "concurrent_writer": False,
        "policy_fingerprint": "policy-1",
        "usage": {
            "codex_usage_known": False,
            "codex_total_tokens": None,
            "gemini_usage_known": False,
            "deepseek_usage_known": False,
            "claude_usage_known": False,
        },
    }
    value.update(changes)
    return value


def _write_aggregates(state_root, *aggregates):
    tasks = state_root / "telemetry" / "tasks"
    tasks.mkdir(parents=True)
    for aggregate in aggregates:
        (tasks / f"{aggregate['task_id']}.json").write_text(
            json.dumps(aggregate), encoding="utf-8"
        )


def test_same_aggregate_is_byte_identical_and_uses_atomic_replace(tmp_path, monkeypatch):
    aggregate = _aggregate()
    vault = tmp_path / "vault"
    replacements = []
    original_replace = projection.os.replace

    def recording_replace(source, target):
        replacements.append((Path(source), Path(target)))
        original_replace(source, target)

    monkeypatch.setattr(projection.os, "replace", recording_replace)
    first = projection.project_task(aggregate, vault_root=vault)
    first_bytes = first.read_bytes()
    projection.project_task(aggregate, vault_root=vault)

    assert first.read_bytes() == first_bytes
    assert len(replacements) == 2
    assert replacements[0][1] == first
    assert not replacements[0][0].exists()
    rendered = first.read_text(encoding="utf-8")
    assert 'repo: "repo"' in rendered
    assert 'branch: "main"' in rendered
    assert 'validation: "PASS"' in rendered
    assert "Validation: PASS" in rendered
    assert 'started_at: "2026-08-30T11:00:00+03:00"' in rendered
    assert 'started_at_utc: "2026-08-30T08:00:00Z"' in rendered
    for usage_flag in ("codex_usage_known", "gemini_usage_known", "deepseek_usage_known", "claude_usage_known"):
        assert f"  {usage_flag}:" in rendered


def test_private_usage_keys_are_not_published_and_public_nulls_remain(tmp_path):
    aggregate = _aggregate()
    aggregate["usage"].update({
        "_claude_5h_start_session_id": "private-session",
        "_claude_5h_end_session_id": "private-session",
        "codex_input_tokens": None,
    })

    rendered = projection.render_run_note(aggregate)

    assert "_claude_" not in rendered
    assert "  codex_input_tokens: null" in rendered


def test_durations_are_human_readable_in_body_and_metrics_but_numeric_in_frontmatter():
    aggregate = _aggregate(duration_ms=1_150_967)

    note = projection.render_run_note(aggregate)
    metrics = projection.render_metrics([aggregate])

    assert "duration_ms: 1150967" in note
    assert "Duration: 19m 11.0s" in note
    assert "1150967 ms" not in note
    assert "Duration median: 19m 11.0s (n=1)" in metrics
    assert "Duration p90: 19m 11.0s (n=1)" in metrics
    assert "1150967 ms" not in metrics


def test_hostile_identifiers_are_normalized_or_rejected_and_stay_under_runs(tmp_path):
    raw_task_id = "../C:\\absolute\\escape\"\n---"
    raw_branch = "--branch: \"bad\"\n---\nforged: true — 日本語"
    task_id = schema.normalize_identifier(raw_task_id)
    branch = schema.normalize_identifier(raw_branch)
    vault = tmp_path / "vault"
    path = projection.project_task(
        _aggregate(task_id=task_id, branch=branch), vault_root=vault
    )

    assert path.resolve().is_relative_to((vault / "Runs").resolve())
    text = path.read_text(encoding="utf-8")
    assert "forged:" not in text
    assert text.count("\n---\n") == 1
    assert f"task_id: {json.dumps(task_id)}" in text
    with pytest.raises(projection.ProjectionError):
        projection.render_run_note(_aggregate(task_id=raw_task_id, branch=raw_branch))


@pytest.mark.parametrize("raw_task_id", ["../escape", r"C:\\absolute\\escape", "CON"])
def test_filename_normalization_cannot_escape_runs_tree(tmp_path, raw_task_id):
    path = projection.project_task(
        _aggregate(task_id=schema.normalize_identifier(raw_task_id)),
        vault_root=tmp_path / "vault",
    )
    assert path.resolve().is_relative_to((tmp_path / "vault" / "Runs").resolve())
    assert path.parent.name == "08"


def test_null_usage_is_null_and_is_not_averaged_as_zero(tmp_path):
    first = _aggregate()
    second = _aggregate(
        task_id="task-2",
        ended_at="2026-08-30T08:00:02Z",
        usage={"codex_usage_known": True, "codex_total_tokens": 100},
    )
    metrics = projection.render_metrics([first, second])
    provider = projection.render_provider_capacity([first, second])

    assert "codex_total_tokens" not in metrics
    assert "Observed total tokens: 100 (n=1)" in provider
    assert "50" not in provider
    assert "summaries:" not in projection._base_efficiency()


def test_every_rate_has_n_and_small_denominators_are_unknown():
    metrics = projection.render_metrics([_aggregate()])
    rate_lines = [
        line for line in metrics.splitlines()
        if "rate:" in line.lower() or "share:" in line.lower()
    ]
    assert rate_lines
    assert all("n=" in line for line in rate_lines)
    assert "minimum n=2" in metrics


def test_disabled_projection_does_not_change_telemetry_or_create_vault(tmp_path):
    telemetry.emit(
        "IMPLEMENTER_FAILED",
        task_id="disabled",
        state_root=tmp_path,
        session_id="session",
        failure_code="CODEX_CALL_FAILED",
    )
    telemetry.update_task(
        "disabled", state_root=tmp_path, projection_enabled=False, vault_root=tmp_path / "vault"
    )
    before = {
        path.relative_to(tmp_path).as_posix(): path.read_bytes()
        for path in (tmp_path / "telemetry").rglob("*")
        if path.is_file()
    }
    telemetry.update_task(
        "disabled", state_root=tmp_path, projection_enabled=False, vault_root=tmp_path / "vault"
    )
    after = {
        path.relative_to(tmp_path).as_posix(): path.read_bytes()
        for path in (tmp_path / "telemetry").rglob("*")
        if path.is_file()
    }
    assert before == after
    assert not (tmp_path / "vault").exists()


def test_projection_root_and_enablement_are_configurable(tmp_path, monkeypatch):
    config_path = tmp_path / "config.json"
    config_path.write_text(
        json.dumps({
            "obsidian_projection": {"enabled": True, "vault_root": str(tmp_path / "configured-vault")}
        }),
        encoding="utf-8",
    )
    monkeypatch.setattr(projection, "_CONFIG_PATH", config_path)
    monkeypatch.setattr(telemetry, "_CONFIG_PATH", config_path)

    assert projection._vault_root(None) == tmp_path / "configured-vault"
    assert telemetry._projection_settings() == (True, tmp_path / "configured-vault")


def test_projection_failure_is_fail_open_and_marks_health_degraded(tmp_path, monkeypatch):
    telemetry.emit(
        "IMPLEMENTER_FAILED",
        task_id="projection-failure",
        state_root=tmp_path,
        session_id="session",
        failure_code="CODEX_CALL_FAILED",
    )

    def fail(*_args, **_kwargs):
        raise RuntimeError("must not escape")

    monkeypatch.setattr(telemetry.obsidian_projection, "project_task", fail)
    assert telemetry.update_task(
        "projection-failure",
        state_root=tmp_path,
        projection_enabled=True,
        vault_root=tmp_path / "vault",
    ) is None
    health = json.loads(
        (tmp_path / "telemetry" / "telemetry_health.json").read_text(encoding="utf-8")
    )
    assert health["status"] == "DEGRADED"
    assert health["last_error_code"] == "TELEMETRY_PROJECTION_FAILED"


def test_rebuild_writes_all_projection_files_only_to_vault(tmp_path):
    state_root = tmp_path / "state"
    vault = tmp_path / "vault"
    _write_aggregates(state_root, _aggregate())
    paths = projection.rebuild(state_root=state_root, vault_root=vault)

    expected = {
        "Dashboard.md",
        "Runs/2026/08/task-1.md",
        "Calibration/Current Metrics.md",
        "Providers/Current Capacity.md",
        "System/Telemetry Health.md",
        "System/README.md",
        "Views/Runs.base",
        "Views/Routing.base",
        "Views/Efficiency.base",
    }
    assert {path.relative_to(vault).as_posix() for path in paths} == expected
    assert all(path.resolve().is_relative_to(vault.resolve()) for path in paths)
    assert not (vault / ".obsidian").exists()
    before = {path.relative_to(vault).as_posix(): path.read_bytes() for path in paths}
    projection.rebuild(state_root=state_root, vault_root=vault)
    after = {path.relative_to(vault).as_posix(): path.read_bytes() for path in paths}
    assert after == before


def test_human_note_is_never_overwritten(tmp_path):
    aggregate = _aggregate()
    path = tmp_path / "vault" / "Runs" / "2026" / "08" / "task-1.md"
    path.parent.mkdir(parents=True)
    path.write_text("# Human note\n", encoding="utf-8")
    with pytest.raises(projection.ProjectionConflict):
        projection.project_task(aggregate, vault_root=tmp_path / "vault")
    assert path.read_text(encoding="utf-8") == "# Human note\n"
