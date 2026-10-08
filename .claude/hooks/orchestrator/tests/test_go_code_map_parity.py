import json
import os
from pathlib import Path
import stat
import subprocess
import sys
from types import SimpleNamespace

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "code_map.json"
sys.path.insert(0, str(ORCHESTRATOR))
import code_map


def _repo(path):
    path.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=path, check=True)
    return path


@pytest.mark.parametrize("status", ["FRESH", "STALE", "UNAVAILABLE"])
def test_p44_python_cli_preserves_pre_status_line(tmp_path, monkeypatch, capsys, status):
    repo = _repo(tmp_path / f"p44_{status.lower()}")
    map_path = Path("C:/Users/test/code map/map.txt")
    calls = []

    def refresh(root, **kwargs):
        calls.append((root, kwargs))
        return map_path, status

    monkeypatch.setattr(code_map, "refresh", refresh)
    monkeypatch.setattr(sys, "argv", ["code_map.py", "--repo", str(repo), "--budget-seconds", "0.2"])
    assert code_map.main() == 0
    assert calls == [(repo.resolve(), {"budget_seconds": 0.2})]
    assert capsys.readouterr().out == f"CODE_MAP={map_path.resolve()} STATUS={status}\n"


def test_p43_python_oracle_inventory_cache_and_bounds(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python .claude/hooks/orchestrator/code_map.py inventory and refresh cache"
    assert fixture["limits"] == {
        "max_files": code_map.MAX_FILES,
        "max_file_bytes": code_map.MAX_FILE_BYTES,
        "max_total_bytes": code_map.MAX_TOTAL_BYTES,
        "max_map_bytes": code_map.MAX_MAP_BYTES,
        "max_seconds": code_map.MAX_SECONDS,
    }
    monkeypatch.setattr(code_map, "_backend", lambda: ("unavailable", None))
    for case in fixture["cases"]:
        assert "p43" in case["name"]
        if case["op"] == "inventory":
            repo = _repo(tmp_path / case["name"])
            if case["kind"] == "symlink-internal":
                (repo / "target.txt").write_text("inside", encoding="utf-8")
                (repo / "alias.txt").write_text("symlink target placeholder", encoding="utf-8")
                expected_paths = {"target.txt", "alias.txt"}
                alias = repo / "alias.txt"
            elif case["kind"] == "symlink-escape":
                outside = tmp_path / f"{case['name']}-outside.txt"
                outside.write_text("outside", encoding="utf-8")
                (repo / "escape.txt").write_text("symlink target placeholder", encoding="utf-8")
                expected_paths = {"escape.txt"}
                alias = repo / "escape.txt"
            else:
                (repo / "directory").write_text("tracked", encoding="utf-8")
                subprocess.run(["git", "add", "directory"], cwd=repo, check=True)
                (repo / "directory").unlink()
                (repo / "directory").mkdir()
                expected_paths = {"directory"}
                alias = repo / "directory"
            if case["kind"].startswith("symlink-"):
                original_lstat = Path.lstat

                def symlink_lstat(path):
                    info = original_lstat(path)
                    if path == alias:
                        return SimpleNamespace(st_mode=stat.S_IFLNK, st_mtime_ns=info.st_mtime_ns,
                                               st_size=info.st_size)
                    return info

                with monkeypatch.context() as patch:
                    patch.setattr(Path, "lstat", symlink_lstat)
                    path, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
            else:
                path, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
            assert status == "FRESH", case["name"]
            entries = json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]
            assert set(entries) == expected_paths, case["name"]
            assert entries[alias.relative_to(repo).as_posix()]["language"] == case["expected_entry"]
            assert entries[alias.relative_to(repo).as_posix()]["coverage"] == "unavailable"
        elif case["op"] == "cache":
            repo = _repo(tmp_path / case["name"])
            (repo / "sample.py").write_text("x = 1\n", encoding="utf-8")
            state = tmp_path / f"{case['name']}-state"
            path, status = code_map.refresh(repo, state)
            assert status == "FRESH"
            manifest_path = path.with_name("manifest.json")
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            manifest.update(version=case["version"], parser=case["parser"])
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            rebuilt, status = code_map.refresh(repo, state)
            refreshed = json.loads(rebuilt.with_name("manifest.json").read_text(encoding="utf-8"))
            assert status == case["refresh_status"], case["name"]
            assert refreshed["version"] == fixture["version"]
            assert refreshed["parser"] == fixture["parser"]
        elif case["op"] == "cache-key":
            repo = _repo(tmp_path / case["name"])
            source = repo / "sample.py"
            source.write_bytes(b"x = 1\n##\n")
            info = source.stat()
            assert info.st_size == 9
            state = tmp_path / f"{case['name']}-state"
            path, status = code_map.refresh(repo, state)
            assert status == "FRESH"
            first = json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]["sample.py"]
            assert first["mtime_ns"] == info.st_mtime_ns and first["size"] == 9
            if case["expected_hit"]:
                with monkeypatch.context() as patch:
                    patch.setattr(code_map, "_python_symbols", lambda _: pytest.fail("matching cache key reparsed"))
                    code_map.refresh(repo, state)
            else:
                source.write_bytes(b"x = 1\n#xx\n")
                os.utime(source, ns=(info.st_atime_ns, info.st_mtime_ns))
                assert source.stat().st_mtime_ns == info.st_mtime_ns
                path, status = code_map.refresh(repo, state)
                updated = json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]["sample.py"]
                assert status == "FRESH" and updated["size"] == case["size"]
        elif case["op"] == "limit":
            if "expected_error" in case:
                repo = _repo(tmp_path / case["name"])
                (repo / "a.bin").write_bytes(b"a" * (16 * 1024 * 1024))
                (repo / "b.bin").write_bytes(b"b" * (16 * 1024 * 1024))
                (repo / "c.bin").write_bytes(b"c" * case["size"])
                _, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
                assert status == case["refresh_status"], case["name"]
                assert case["total"] == code_map.MAX_TOTAL_BYTES
            else:
                repo = _repo(tmp_path / case["name"])
                (repo / "sample.bin").write_bytes(b"x" * case["size"])
                path, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
                manifest = json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))
                assert status == "FRESH", case["name"]
                assert ("sample.bin" not in manifest["entries"]) is case["expected_skip"]
                assert manifest["skipped_files"] == int(case["expected_skip"])
        elif case["op"] == "file-count":
            repo = _repo(tmp_path / case["name"])
            for index in range(case["count"]):
                (repo / f"f{index:04}.txt").touch()
            _, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
            assert status == case["refresh_status"], case["name"]
            assert case["count"] > code_map.MAX_FILES
        elif case["op"] == "oracle":
            repo = _repo(tmp_path / case["name"])
            for relative, source in case["sources"].items():
                path = repo / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(source, encoding="utf-8")
            subprocess.run(["git", "add", "--", *case["sources"]], cwd=repo, check=True)
            path, status = code_map.refresh(repo, tmp_path / f"{case['name']}-state")
            assert status == "FRESH", case["name"]
            assert path.read_text(encoding="utf-8") == case["expected_map"], case["name"]
            entries = json.loads(path.with_name("manifest.json").read_text(encoding="utf-8"))["entries"]
            for relative, symbols in case["expected_symbols"].items():
                assert entries[relative]["symbols"] == symbols, case["name"]
