import io
from pathlib import Path
import shutil
import sys
import uuid

import pytest


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import purser_optimize as optimize


@pytest.fixture
def tmp_path():
    """Use a unique .tmp workspace because the host temp ACL is unreadable here."""
    scratch = Path(__file__).resolve().parents[4] / ".tmp"
    path = scratch / f"purser-optimize-{uuid.uuid4().hex}"
    path.mkdir(parents=True, exist_ok=True)
    try:
        yield path
    finally:
        shutil.rmtree(path)


def test_plain_output_reconfigures_cp1254_stdout_and_is_ascii(tmp_path, monkeypatch):
    output = io.BytesIO()
    stream = io.TextIOWrapper(output, encoding="cp1254")
    monkeypatch.setattr(optimize.sys, "stdout", stream)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    assert optimize.main(["--projects-root", str(tmp_path / "projects")]) == 0
    stream.flush()
    assert stream.encoding.lower() == "utf-8"
    assert "> ⚓ **Purser optimize**" in output.getvalue().decode("utf-8")
