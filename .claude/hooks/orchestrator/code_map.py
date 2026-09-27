"""On-demand, bounded code map for a Git work tree."""

from __future__ import annotations

import argparse
import ast
import importlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time


VERSION = 4
MAX_FILES = 5_000
MAX_FILE_BYTES = 512 * 1024
MAX_TOTAL_BYTES = 32 * 1024 * 1024
MAX_MAP_BYTES = 256 * 1024
MAX_SECONDS = 10
LANGUAGES = {
    ".js": "javascript", ".jsx": "javascript", ".ts": "typescript",
    ".tsx": "tsx", ".go": "go", ".rs": "rust", ".java": "java",
    ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp",
    ".hpp": "cpp", ".cs": "c_sharp", ".rb": "ruby", ".php": "php",
    ".swift": "swift", ".kt": "kotlin", ".kts": "kotlin",
    ".scala": "scala", ".lua": "lua", ".sh": "bash", ".bash": "bash",
}
KINDS = {"function", "method", "class", "type", "interface", "enum", "struct", "trait"}
TYPESCRIPT_TAGS = """
(function_declaration name: (identifier) @name) @definition.function
(class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type
(method_definition name: (property_identifier) @name) @definition.method
(variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)]) @definition.function
"""


class MapLimit(Exception):
    pass


def _check(deadline: float) -> None:
    if time.monotonic() >= deadline:
        raise MapLimit("time limit")


def _signature(node: ast.FunctionDef | ast.AsyncFunctionDef) -> str:
    args = ast.unparse(node.args)
    result = f"({args})"
    if node.returns:
        result += f" -> {ast.unparse(node.returns)}"
    result = " ".join(result.split())
    return result if len(result) <= 160 else result[:159] + "…"


def _python_symbols(source: bytes) -> list[list[object]]:
    tree = ast.parse(source.decode("utf-8-sig"))
    found: list[list[object]] = []

    def visit(body: list[ast.stmt], parents: tuple[str, ...], in_class: bool) -> None:
        for node in body:
            if isinstance(node, ast.ClassDef):
                name = ".".join((*parents, node.name))
                found.append([node.lineno, "class", name, ""])
                visit(node.body, (*parents, node.name), True)
            elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                name = ".".join((*parents, node.name))
                found.append([node.lineno, "method" if in_class else "function", name, _signature(node)])
                visit(node.body, (*parents, node.name), False)
            else:
                visit([child for child in ast.iter_child_nodes(node) if isinstance(child, ast.stmt)], parents, in_class)

    visit(tree.body, (), False)
    return sorted(found, key=lambda item: (item[0], item[2]))


def _is_test_file(path: str) -> bool:
    parts = path.lower().split("/")
    name = parts[-1]
    return (any(part in {"tests", "test", "__tests__"} for part in parts[:-1])
            or (name.startswith("test_") or name.endswith("_test.py")) and name.endswith(".py")
            or ".test." in name or ".spec." in name)


def _test_count(source: bytes, language: str) -> int:
    if language == "python":
        tree = ast.parse(source.decode("utf-8-sig"))
        return sum(isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith("test_")
                   for node in ast.walk(tree))
    if language == "unknown":
        return 0
    return len(re.findall(rb"(?m)^\s*(?:test|it)\s*\(|^\s*(?:func|fn|function)\s+(?:Test[A-Z]|test_)\w*\s*\(", source))


def _backend():
    for name in ("tree_sitter_language_pack", "tree_sitter_languages"):
        try:
            return name, importlib.import_module(name)
        except ImportError:
            pass
    return "unavailable", None


def _tree_symbols(source: bytes, language: str, backend: object) -> list[list[object]] | None:
    """Use definition queries, including TypeScript forms absent from bundled tags."""
    stage = "parser"
    try:
        if hasattr(backend, "has_parser") and not backend.has_parser(language):
            return None
        try:
            parser = backend.get_parser(language)
            grammar = backend.get_language(language)
        except (LookupError, ValueError, TypeError):
            return None
        stage = "parse"
        root = parser.parse(source).root_node
        def compile_query(pattern: str):
            try:
                return grammar.query(pattern)
            except AttributeError:
                from tree_sitter import Query  # optional, inside the indexer only
                return Query(grammar, pattern)

        stage = "query"
        tags = backend.get_tags_query(language) if hasattr(backend, "get_tags_query") else None
        if language in {"typescript", "tsx"}:
            tags = (tags or "") + TYPESCRIPT_TAGS
        if not tags:
            patterns = []
            node_types = (grammar.node_kind_for_id(index) for index in range(grammar.node_kind_count))
            for node_type in node_types:
                if not node_type or not node_type.endswith(("_declaration", "_definition", "_item", "_statement")):
                    continue
                kind = next((part for part in ("method", "function", "class", "interface", "struct",
                                              "enum", "trait", "type") if part in node_type), None)
                if kind is None:
                    continue
                pattern = f"({node_type} name: (_) @name) @definition.{kind}"
                try:
                    compile_query(pattern)
                    patterns.append(pattern)
                except Exception:
                    pass
            tags = "\n".join(patterns)
        if not tags:
            return None
        query = compile_query(tags)
        try:
            from tree_sitter import QueryCursor
            captures = QueryCursor(query).captures(root)
        except (ImportError, TypeError):
            captures = query.captures(root)
        pairs = ([(node, key) for key, nodes in captures.items() for node in nodes]
                 if isinstance(captures, dict) else captures)
        definitions = [(node, label.split(".", 1)[1]) for node, label in pairs
                       if label.startswith("definition.") and label.split(".", 1)[1] in KINDS]
        names = [node for node, label in pairs if label == "name"]
        found: list[list[object]] = []
        for node, kind in definitions:
            candidates = [name for name in names if node.start_byte <= name.start_byte
                          and name.end_byte <= node.end_byte]
            if not candidates:
                continue
            name_node = min(candidates, key=lambda item: (item.start_byte, item.end_byte))
            name = source[name_node.start_byte:name_node.end_byte].decode("utf-8", "replace").strip()
            if not name or "\n" in name:
                continue
            parents = []
            for parent, parent_kind in definitions:
                if parent is node or parent_kind not in {"class", "struct", "interface", "trait", "enum"}:
                    continue
                if parent.start_byte < node.start_byte and node.end_byte <= parent.end_byte:
                    parent_names = [part for part in names if parent.start_byte <= part.start_byte
                                    and part.end_byte <= parent.end_byte]
                    if parent_names:
                        first = min(parent_names, key=lambda item: item.start_byte)
                        parents.append((parent.end_byte - parent.start_byte,
                                        source[first.start_byte:first.end_byte].decode("utf-8", "replace")))
            qualified = ".".join([part for _, part in sorted(parents, reverse=True)] + [name])
            if kind == "function" and parents:
                kind = "method"
            found.append([name_node.start_point[0] + 1, kind, qualified, ""])
        if language in {"typescript", "tsx"}:
            for node in root.named_children:
                if node.type == "export_statement" and source[node.start_byte:node.end_byte].startswith(b"export default"):
                    for child in node.named_children:
                        if child.type in {"function_expression", "arrow_function"}:
                            found.append([child.start_point[0] + 1, "function", "default", ""])
        return sorted({tuple(item) for item in found}, key=lambda item: (item[0], item[2]))
    except Exception as exc:
        raise OSError(f"{stage} failed: {type(exc).__name__}") from exc


def _inventory(repo: Path, deadline: float) -> list[str]:
    _check(deadline)
    result = subprocess.run(["git", "ls-files", "-co", "--exclude-standard", "-z"],
                            cwd=repo, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            timeout=max(0.01, deadline - time.monotonic()), check=True)
    paths = sorted(set(os.fsdecode(name) for name in result.stdout.split(b"\0") if name))
    if len(paths) > MAX_FILES:
        raise MapLimit("file count")
    return paths


def _render(entries: dict[str, dict], backend_name: str) -> bytes:
    covered = sorted({entry["language"] for entry in entries.values() if entry["coverage"] != "unavailable"})
    lines = [f"# Code map v{VERSION}", "Parser: Python ast; other languages: " + backend_name,
             "Covered languages: " + (", ".join(covered) if covered else "none"), ""]
    for path, entry in sorted(entries.items()):
        if _is_test_file(path):
            lines.append(f"{path}  ({entry.get('tests', 0)} tests)")
            continue
        lines.append(path)
        if entry["coverage"] == "unavailable" and entry["language"] not in {"unknown", "python"}:
            lines.append(f"  symbols: unavailable ({entry.get('reason', 'install tree-sitter-language-pack')})")
        for line, kind, name, signature in entry["symbols"]:
            rendered = f"  {line} {kind} {name}{signature}"
            lines.append(rendered if not signature or len(rendered) <= 120 else rendered[:119] + "…")
    data = ("\n".join(lines) + "\n").encode("utf-8")
    if len(data) > MAX_MAP_BYTES:
        raise MapLimit("rendered map")
    return data


def refresh(repo: Path, state: Path | None = None) -> tuple[Path, str]:
    """Return the fixed map path and its honest freshness status."""
    import project_root
    import state_root
    from ledger import _lease_lock
    from compaction_advisor import _atomic_write

    repo = repo.resolve()
    root = state if state is not None else state_root.default_state_root()
    directory = root / "projects" / project_root.project_key(repo) / "code-map"
    target = directory / "map.txt"
    manifest_path = directory / "manifest.json"
    deadline = time.monotonic() + MAX_SECONDS
    try:
        with _lease_lock(directory / "index.lock"):
            _check(deadline)
            backend_name, backend = _backend()
            try:
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
                if manifest.get("version") != VERSION or manifest.get("parser") != backend_name or not target.is_file():
                    manifest = {}
            except (OSError, ValueError, TypeError):
                manifest = {}
            old = manifest.get("entries", {})
            if not isinstance(old, dict):
                old = {}
            entries: dict[str, dict] = {}
            total = 0
            for relative in _inventory(repo, deadline):
                _check(deadline)
                path = repo / relative
                try:
                    info = path.lstat()
                except FileNotFoundError:
                    continue  # git ls-files still reports a deleted tracked path
                if not stat.S_ISREG(info.st_mode) or not path.resolve().is_relative_to(repo):
                    entries[relative] = {"mtime_ns": info.st_mtime_ns, "size": info.st_size,
                                         "language": "unknown", "coverage": "unavailable", "symbols": []}
                    continue
                if info.st_size > MAX_FILE_BYTES:
                    raise MapLimit("file size")
                total += info.st_size
                if total > MAX_TOTAL_BYTES:
                    raise MapLimit("total size")
                prior = old.get(relative)
                if isinstance(prior, dict) and prior.get("mtime_ns") == info.st_mtime_ns and prior.get("size") == info.st_size:
                    entries[relative] = prior
                    continue
                language = "python" if path.suffix.lower() in {".py", ".pyi"} else LANGUAGES.get(path.suffix.lower(), "unknown")
                symbols: list[list[object]] = []
                coverage = "unavailable"
                reason = "parser or definition query unavailable"
                test_file = _is_test_file(relative)
                tests = 0
                if test_file or language == "python" or (backend is not None and language != "unknown"):
                    source = path.read_bytes()
                    if len(source) > MAX_FILE_BYTES:
                        raise MapLimit("file size")
                    after = path.stat()
                    if after.st_mtime_ns != info.st_mtime_ns or after.st_size != info.st_size:
                        raise OSError("file changed during indexing")
                    if test_file:
                        try:
                            tests = _test_count(source, language)
                            if language == "python":
                                coverage = "ast"
                        except (SyntaxError, UnicodeError, ValueError):
                            pass
                    elif language == "python":
                        try:
                            symbols = _python_symbols(source)
                            coverage = "ast"
                        except (SyntaxError, UnicodeError, ValueError):
                            pass
                    else:
                        try:
                            parsed = _tree_symbols(source, language, backend)
                            if parsed is not None:
                                symbols, coverage = parsed, "tree-sitter"
                            else:
                                reason = "parser or definition query unavailable"
                        except OSError as exc:
                            reason = str(exc)
                entries[relative] = {"mtime_ns": info.st_mtime_ns, "size": info.st_size,
                                     "language": language, "coverage": coverage, "symbols": symbols}
                if language != "python" and backend is not None and language != "unknown" and coverage == "unavailable":
                    entries[relative]["reason"] = reason
                if test_file:
                    entries[relative]["tests"] = tests
            data = _render(entries, backend_name)
            _check(deadline)
            if not manifest or entries != old or not target.is_file():
                _atomic_write(target, data)
                _atomic_write(manifest_path, json.dumps({"version": VERSION, "parser": backend_name,
                            "entries": entries}, ensure_ascii=False, sort_keys=True).encode("utf-8"))
            return target.resolve(), "FRESH"
    except (OSError, ValueError, subprocess.SubprocessError, MapLimit):
        return target.resolve(), "STALE" if target.is_file() else "UNAVAILABLE"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    args = parser.parse_args()
    repo = args.repo.resolve()
    try:
        result = subprocess.run(["git", "rev-parse", "--show-toplevel"], cwd=repo,
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=2, check=True)
        repo = Path(os.fsdecode(result.stdout.strip())).resolve()
    except (OSError, subprocess.SubprocessError):
        import project_root
        import state_root
        target = state_root.default_state_root() / "projects" / project_root.project_key(repo) / "code-map" / "map.txt"
        print(f"CODE_MAP={target} STATUS=UNAVAILABLE")
        return 1
    path, status = refresh(repo)
    print(f"CODE_MAP={path} STATUS={status}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
