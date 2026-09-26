import json
from pathlib import Path
import subprocess

import pytest

import code_map


def _repo(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True)
    return repo


def _entries(repo):
    path, status = code_map.refresh(repo)
    assert status == "FRESH"
    return path, json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]


def test_reuses_unchanged_entries(tmp_path, monkeypatch):
    repo = _repo(tmp_path)
    (repo / "a.py").write_text("def one(x: int):\n    return x\n", encoding="utf-8")
    (repo / "tests").mkdir()
    (repo / "tests" / "test_sample.py").write_text("def test_one(): pass\ndef test_two(): pass\n", encoding="utf-8")
    path, first = _entries(repo)
    assert path.read_text(encoding="utf-8").endswith("tests/test_sample.py  (2 tests)\n")
    before = path.stat().st_mtime_ns
    monkeypatch.setattr(code_map, "_python_symbols", lambda _: pytest.fail("unchanged file reparsed"))
    again, second = _entries(repo)
    assert again == path and first == second and path.stat().st_mtime_ns == before


def test_dirty_file_refreshes_without_head_change(tmp_path):
    repo = _repo(tmp_path)
    source = repo / "a.py"
    source.write_text("def one():\n    pass\n", encoding="utf-8")
    subprocess.run(["git", "add", "a.py"], cwd=repo, check=True)
    _entries(repo)
    source.write_text("def two(arg):\n    pass\n", encoding="utf-8")
    path, entries = _entries(repo)
    assert "two(arg)" in path.read_text(encoding="utf-8")
    assert entries["a.py"]["symbols"][0][2] == "two"


def test_deleted_file_drops_from_map(tmp_path):
    repo = _repo(tmp_path)
    source = repo / "gone.py"
    source.write_text("class Gone:\n    pass\n", encoding="utf-8")
    subprocess.run(["git", "add", "gone.py"], cwd=repo, check=True)
    _entries(repo)
    source.unlink()
    path, entries = _entries(repo)
    assert "gone.py" not in entries and "gone.py" not in path.read_text(encoding="utf-8")


def test_limit_preserves_complete_map(tmp_path, monkeypatch):
    repo = _repo(tmp_path)
    (repo / "small.py").write_text("def okay():\n    pass\n", encoding="utf-8")
    path, _ = _entries(repo)
    before = path.read_bytes()
    monkeypatch.setattr(code_map, "MAX_FILE_BYTES", 1)
    assert code_map.refresh(repo) == (path, "STALE")
    assert path.read_bytes() == before


def test_unsupported_language_without_parser_is_listed(tmp_path, monkeypatch):
    repo = _repo(tmp_path)
    (repo / "sample.js").write_text("function hello() {}\n", encoding="utf-8")
    (repo / "notes.txt").write_text("not source code\n", encoding="utf-8")
    monkeypatch.setattr(code_map, "_backend", lambda: ("unavailable", None))
    path, _ = _entries(repo)
    data = path.read_text(encoding="utf-8")
    assert "sample.js\n  symbols: unavailable (install tree-sitter-language-pack)" in data
    assert "notes.txt\nsample.js\n" in data
    assert "Covered languages: none" in data


def test_tree_sitter_when_installed(tmp_path):
    name, backend = code_map._backend()
    if backend is None or not hasattr(backend, "get_tags_query"):
        pytest.skip("tree-sitter language pack with tags unavailable")
    repo = _repo(tmp_path)
    (repo / "sample.js").write_text("function hello() {}\n", encoding="utf-8")
    path, entries = _entries(repo)
    if not backend.get_tags_query("javascript"):
        pytest.skip("JavaScript tags query unavailable")
    assert entries["sample.js"]["coverage"] == "tree-sitter"
    assert "function hello" in path.read_text(encoding="utf-8")
