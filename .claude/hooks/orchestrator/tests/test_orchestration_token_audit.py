import importlib.util
import shutil
from pathlib import Path
import uuid

import pytest


SCRIPT = Path(__file__).parents[4] / "scripts" / "orchestration_token_audit_0.py"
SPEC = importlib.util.spec_from_file_location("orchestration_token_audit_0", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


@pytest.fixture()
def tmp_path():
    root = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "audit"
        / uuid.uuid4().hex
    )
    root.mkdir(parents=True)
    try:
        yield root
    finally:
        shutil.rmtree(root)


def test_project_slug_matches_claude_code_windows_example():
    assert audit._project_slug(r"C:\Projects\purser") == "C--Projects-purser"


def test_transcript_glob_uses_derived_project_slug(monkeypatch, tmp_path):
    monkeypatch.setattr(audit.Path, "home", lambda: tmp_path)

    value = audit._transcript_glob(r"C:\Projects\purser")

    assert value == str(
        tmp_path / ".claude" / "projects" / "C--Projects-purser" / "*.jsonl"
    )
