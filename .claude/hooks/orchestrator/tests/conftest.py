from pathlib import Path
import shutil
import sys
import uuid

import pytest


orchestrator_dir = str(Path(__file__).resolve().parents[1])
if orchestrator_dir not in sys.path:
    sys.path.insert(0, orchestrator_dir)


@pytest.fixture
def tmp_path():
    """Keep pytest scratch in the repository's ignored .tmp directory."""
    scratch_root = Path(__file__).parents[4] / ".tmp"
    scratch_root.mkdir(parents=True, exist_ok=True)
    scratch = scratch_root / f"pytest-{uuid.uuid4().hex}"
    scratch.mkdir()
    try:
        yield scratch
    finally:
        if scratch.parent == scratch_root:
            shutil.rmtree(scratch, ignore_errors=True)


@pytest.fixture(autouse=True)
def isolated_purser_state_root(tmp_path, monkeypatch):
    """Keep tests that exercise default paths away from the user's real state."""
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path / "purser-state"))
