from pathlib import Path
import os
import shutil
import subprocess
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
    monkeypatch.setenv("CLAUDE_PLUGIN_ROOT", str(Path(__file__).parents[4]))
    monkeypatch.delenv("PURSER_OBSIDIAN_VAULT", raising=False)
    codex_scratch = tmp_path.parent / f"pytest-codex-{uuid.uuid4().hex}"
    fake_bin = codex_scratch / "bin"
    fake_bin.mkdir(parents=True)
    fake_codex = fake_bin / ("codex.cmd" if os.name == "nt" else "codex")
    fake_codex.write_text("@exit /b 1\r\n" if os.name == "nt" else "#!/bin/sh\nexit 1\n")
    if os.name != "nt":
        fake_codex.chmod(0o755)
    monkeypatch.setenv("CODEX_HOME", str(codex_scratch / "codex-home"))
    monkeypatch.setenv("PATH", str(fake_bin) + os.pathsep + os.environ.get("PATH", ""))
    try:
        yield
    finally:
        if codex_scratch.parent == tmp_path.parent:
            shutil.rmtree(codex_scratch, ignore_errors=True)


@pytest.fixture(autouse=True)
def prevent_detached_mechanical_benchmarks(monkeypatch, request):
    """Fail if a test tries to launch the detached OpenCode benchmark worker."""
    import mechanical_learning

    class GuardedSubprocess:
        def __getattr__(self, name):
            return getattr(subprocess, name)

        def Popen(self, args, *positional, **options):
            if (isinstance(args, (list, tuple)) and len(args) >= 3
                    and Path(args[1]).resolve() == Path(mechanical_learning.__file__).resolve()
                    and args[2] in ("--benchmark", "--shadow")):
                pytest.fail(
                    f"{request.node.nodeid} attempted to launch a detached mechanical benchmark"
                )
            return subprocess.Popen(args, *positional, **options)

    monkeypatch.setattr(mechanical_learning, "subprocess", GuardedSubprocess())
