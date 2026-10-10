import json
import subprocess

import pytest

import code_map


def _repo(tmp_path, name="repo"):
    repo = tmp_path / name
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True)
    return repo


def _entries(repo):
    path, status = code_map.refresh(repo)
    assert status == "FRESH"
    return path, json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]


def _configure_backend(tmp_path):
    _, backend = code_map._backend()
    if backend is None or not hasattr(backend, "get_tags_query"):
        pytest.skip("tree-sitter language pack with tags unavailable")
    backend.configure(backend.PackConfig(cache_dir=str(tmp_path / "parser-cache")))


def test_tree_sitter_when_installed(tmp_path):
    _configure_backend(tmp_path)
    repo = _repo(tmp_path)
    (repo / "sample.ts").write_text(
        "export function named() {}\n"
        "export default function Default() {}\n"
        "export const Arrow = (x: number) => x;\n"
        "export const Func = function () {};\n"
        "export class Widget { run() {} }\n", encoding="utf-8")
    (repo / "sample.tsx").write_text(
        "export const Card = () => <div />;\n"
        "export default function() { return <Card />; }\n", encoding="utf-8")
    path, entries = _entries(repo)
    assert entries["sample.ts"]["coverage"] == entries["sample.tsx"]["coverage"] == "tree-sitter"
    data = path.read_text(encoding="utf-8")
    for symbol in ("function named", "function Default", "function Arrow", "function Func",
                   "class Widget", "method Widget.run", "function Card", "function default"):
        assert symbol in data


def test_tree_sitter_typescript_interface_and_type_symbols(tmp_path):
    _configure_backend(tmp_path)
    repo = _repo(tmp_path)
    (repo / "sample.ts").write_text(
        "export interface Shape { x: number }\n"
        "export type Thing = string;\n", encoding="utf-8")
    path, entries = _entries(repo)
    assert entries["sample.ts"]["coverage"] == "tree-sitter"
    data = path.read_text(encoding="utf-8")
    assert "interface Shape" in data
    assert "type Thing" in data


def _assert_python_test_count_rendering(tmp_path, monkeypatch):
    """Python-only: Go has no test-file count rendering equivalent."""
    repo = _repo(tmp_path, "python-test-count")
    (repo / "a.py").write_text("def one(x: int):\n    return x\n", encoding="utf-8")
    (repo / "tests").mkdir()
    (repo / "tests" / "test_sample.py").write_text("def test_one(): pass\ndef test_two(): pass\n", encoding="utf-8")
    path, first = _entries(repo)
    assert path.read_text(encoding="utf-8").endswith("tests/test_sample.py  (2 tests)\n")
    before = path.stat().st_mtime_ns
    with monkeypatch.context() as patch:
        patch.setattr(code_map, "_python_symbols", lambda _: pytest.fail("unchanged file reparsed"))
        again, second = _entries(repo)
    assert again == path and first == second and path.stat().st_mtime_ns == before


def _assert_forced_parser_unavailable(tmp_path, monkeypatch):
    """Python-only: Go has no equivalent forced parser-backend injection."""
    repo = _repo(tmp_path, "forced-parser-unavailable")
    (repo / "sample.js").write_text("function hello() {}\n", encoding="utf-8")
    (repo / "notes.txt").write_text("not source code\n", encoding="utf-8")
    monkeypatch.setattr(code_map, "_backend", lambda: ("unavailable", None))
    path, _ = _entries(repo)
    data = path.read_text(encoding="utf-8")
    assert "sample.js\n  symbols: unavailable (install tree-sitter-language-pack)" in data
    assert "notes.txt\nsample.js\n" in data
    assert "Covered languages: none" in data


def test_tree_sitter_parser_failure_is_listed(tmp_path, monkeypatch):
    _configure_backend(tmp_path)
    _assert_python_test_count_rendering(tmp_path, monkeypatch)
    repo = _repo(tmp_path)
    (repo / "sample.ts").write_text("export function named() {}\n", encoding="utf-8")
    _entries(repo)

    def broken_parser(*_):
        raise OSError("query failed: QueryError")

    monkeypatch.setattr(code_map, "_tree_symbols", broken_parser)
    (repo / "sample.ts").write_text((repo / "sample.ts").read_text(encoding="utf-8") + "\n", encoding="utf-8")
    path, _ = _entries(repo)
    assert "sample.ts\n  symbols: unavailable (query failed: QueryError)" in path.read_text(encoding="utf-8")
    _assert_forced_parser_unavailable(tmp_path, monkeypatch)
