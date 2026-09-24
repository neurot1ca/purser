"""Plan or apply the Purser first-run user roles and project trust."""

from __future__ import annotations

import argparse
from copy import deepcopy
from datetime import datetime, timezone
import difflib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import tomllib

import compaction_advisor


HEAD = re.compile(r"^\s*\[([^\[\]]+)\]\s*(?:#.*)?$")
KEY = re.compile(r"^\s*([A-Za-z0-9_-]+)\s*=")


def _agent(name: str, executable: str | None = None) -> dict:
    path = executable or shutil.which(name)
    if not path:
        return {"available": False, "path": None, "version": None}
    path = str(Path(path).expanduser())
    if not Path(path).is_file() and shutil.which(path) is None:
        return {"available": False, "path": path, "version": None}
    version = None
    try:
        result = subprocess.run([path, "--version"], capture_output=True, text=True, timeout=5, check=False)
        if result.returncode == 0:
            version = (result.stdout or result.stderr).strip() or None
    except (OSError, subprocess.TimeoutExpired):
        pass
    return {"available": True, "path": path, "version": version}


def detect(config_path: Path, project: Path) -> dict:
    data = tomllib.loads(config_path.read_text(encoding="utf-8")) if config_path.exists() else {}
    reviewer = data.get("reviewer", {})
    configured = reviewer.get("executable") if isinstance(reviewer, dict) else None
    trust = data.get("trust", {})
    roles = data.get("roles", {})
    agy = _agent("agy", configured if isinstance(configured, str) else None)
    if not agy["available"] and configured:
        agy = _agent("agy")
    return {
        "agents": {
            "codex": _agent("codex"),
            "agy": agy,
            "claude": _agent("claude"),
        },
        "roles": roles,
        "trusted": str(project) in trust.get("paths", []) if isinstance(trust, dict) else False,
        "profile_exists": (project / ".orchestrator" / "profile.toml").exists(),
    }


def _span(lines: list[str], section: str) -> tuple[int, int] | None:
    headers = [(i, match.group(1).strip()) for i, line in enumerate(lines) if (match := HEAD.match(line.rstrip("\r\n")))]
    found = [n for n, (_, name) in enumerate(headers) if name == section]
    if not found:
        return None
    n = found[0]
    return headers[n][0], headers[n + 1][0] if n + 1 < len(headers) else len(lines)


def _upsert(text: str, section: str, key: str, value: object, newline: str, *, rendered: str | None = None) -> str:
    lines = text.splitlines(keepends=True)
    rendered = rendered if rendered is not None else f"{key} = {json.dumps(value, ensure_ascii=False)}{newline}"
    span = _span(lines, section)
    if span is None:
        if lines and not lines[-1].endswith(("\n", "\r")):
            lines[-1] += newline
        if lines and lines[-1].strip():
            lines.append(newline)
        lines.extend((f"[{section}]{newline}", rendered))
        return "".join(lines)
    start, end = span
    hits = [i for i in range(start + 1, end) if (match := KEY.match(lines[i])) and match.group(1) == key]
    if len(hits) > 1:
        raise ValueError(f"duplicate [{section}] {key}")
    if hits:
        i = hits[0]
        # A TOML array may span lines. Stop at its matching closing bracket.
        j = i + 1
        if section == "trust" and key == "paths":
            rhs = lines[i].split("=", 1)[1]
            if rhs.lstrip().startswith("["):
                depth, quote, escaped = 0, None, False
                for line_no in range(i, end):
                    part = rhs if line_no == i else lines[line_no]
                    for char in part:
                        if escaped:
                            escaped = False
                        elif quote and char == "\\" and quote == '"':
                            escaped = True
                        elif quote and char == quote:
                            quote = None
                        elif not quote and char in ('"', "'"):
                            quote = char
                        elif not quote and char == "#":
                            break
                        elif not quote and char == "[":
                            depth += 1
                        elif not quote and char == "]":
                            depth -= 1
                    if depth == 0:
                        j = line_no + 1
                        break
                else:
                    raise ValueError("unterminated [trust] paths array")
        lines[i:j] = [rendered]
    else:
        lines.insert(end, rendered)
    return "".join(lines)


def _same_path(left: str, right: Path) -> bool:
    try:
        return os.path.normcase(str(Path(left).resolve())) == os.path.normcase(str(right.resolve()))
    except (OSError, RuntimeError, ValueError):
        return False


def _append_trust_path(text: str, path: str, newline: str) -> str:
    literal = f"'{path}'" if "'" not in path else json.dumps(path, ensure_ascii=False)
    lines = text.splitlines(keepends=True)
    span = _span(lines, "trust")
    if span is None or not any((match := KEY.match(lines[i])) and match.group(1) == "paths" for i in range(span[0] + 1, span[1])):
        return _upsert(text, "trust", "paths", [path], newline, rendered=f"paths = [{literal}]{newline}")
    start, end = span
    hits = [i for i in range(start + 1, end) if (match := KEY.match(lines[i])) and match.group(1) == "paths"]
    if len(hits) != 1:
        raise ValueError("duplicate [trust] paths")
    key_line = hits[0]
    offset = sum(map(len, lines[:key_line])) + lines[key_line].index("=") + 1
    while offset < len(text) and text[offset].isspace():
        offset += 1
    if offset >= len(text) or text[offset] != "[":
        raise ValueError("[trust] paths is not an array")
    depth, quote, escaped, comment = 0, None, False, False
    last_significant = offset
    for index in range(offset, len(text)):
        char = text[index]
        if comment:
            if char in "\r\n":
                comment = False
            continue
        if quote:
            if escaped:
                escaped = False
            elif quote == '"' and char == "\\":
                escaped = True
            elif char == quote:
                quote = None
                last_significant = index
            continue
        if char in ("'", '"'):
            quote = char
        elif char == "#":
            comment = True
        elif char == "[":
            depth += 1
        elif char == "]":
            depth -= 1
            if depth == 0:
                addition = literal if last_significant == offset else (" " if text[last_significant] == "," else ", ") + literal
                return text[:last_significant + 1] + addition + text[last_significant + 1:]
        elif depth == 1 and not char.isspace():
            last_significant = index
    raise ValueError("unterminated [trust] paths array")


def _candidate(original: bytes | None, coder: str, reviewer: str, trust: bool, project: Path) -> bytes:
    text = original.decode("utf-8") if original is not None else "schema_version = 1\n"
    before = tomllib.loads(text)
    newline = "\r\n" if "\r\n" in text else "\n"
    after = deepcopy(before)
    roles = after.setdefault("roles", {})
    if not isinstance(roles, dict):
        raise ValueError("[roles] must be a table")
    roles.update(orchestrator="claude-code", coder=coder, reviewer=reviewer)
    for key in ("orchestrator", "coder", "reviewer"):
        text = _upsert(text, "roles", key, roles[key], newline)
    if trust:
        table = after.setdefault("trust", {})
        if not isinstance(table, dict) or not isinstance(table.get("paths", []), list) or not all(isinstance(p, str) for p in table.get("paths", [])):
            raise ValueError("[trust] paths must be an array of strings")
        paths = table.setdefault("paths", [])
        if not any(_same_path(path, project) for path in paths):
            paths.append(str(project))
            text = _append_trust_path(text, str(project), newline)
    if tomllib.loads(text) != after:
        raise ValueError("config edit changed values outside the requested keys")
    return text.encode("utf-8")


def _profile(project: Path) -> bytes:
    lines = ["schema_version = 1", "[project]", f"id = {json.dumps(project.name)}", "[paths]", "protected = []"]
    if (project / "AGENTS.md").is_file():
        lines.extend(("[authority]", 'sources = [{ path = "AGENTS.md", headings = ["Authority", "Working scope", "Proportional validation", "Test and comment quality"] }]'))
    result = ("\n".join(lines) + "\n").encode("utf-8")
    tomllib.loads(result.decode("utf-8"))
    return result


def _atomic(path: Path, content: bytes, *, exclusive: bool = False) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        if exclusive:
            os.link(temporary, path)
        else:
            os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--detect", action="store_true")
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--apply", action="store_true")
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--coder", choices=("codex", "claude"))
    parser.add_argument("--reviewer", choices=("agy", "none"))
    parser.add_argument("--trust", action="store_true")
    parser.add_argument("--scaffold-profile", action="store_true")
    args = parser.parse_args(argv)
    if args.detect:
        if not args.json or args.coder or args.reviewer or args.trust or args.scaffold_profile:
            parser.error("--detect requires --json and no change options")
    elif args.json or args.coder is None or args.reviewer is None:
        parser.error("--plan and --apply require --coder and --reviewer, without --json")
    project = Path.cwd().resolve()
    config = Path.home() / ".orchestrator" / "config.toml"
    python_file = config.with_name("python")
    profile = project / ".orchestrator" / "profile.toml"
    try:
        if args.detect:
            print(json.dumps(detect(config, project), indent=2))
            return 0
        handoff = compaction_advisor.handoff_path(project)
        original = config.read_bytes() if config.exists() else None
        changed = _candidate(original, args.coder, args.reviewer, args.trust, project)
        scaffold = _profile(project) if args.scaffold_profile and not profile.exists() else None
        handoff_missing = not handoff.exists()
        python_original = python_file.read_bytes() if python_file.exists() else None
        python_changed = (str(Path(sys.executable).resolve()) + "\n").encode("utf-8") if sys.version_info >= (3, 11) else None
        python_update = python_changed is not None and python_original != python_changed
        if args.plan:
            if changed != original:
                print("".join(difflib.unified_diff((original or b"").decode("utf-8").splitlines(keepends=True), changed.decode("utf-8").splitlines(keepends=True), fromfile=str(config), tofile=str(config))), end="")
            if scaffold is not None:
                print(f"Create {profile}:\n{scaffold.decode('utf-8')}", end="")
            if python_update:
                print("".join(difflib.unified_diff((python_original or b"").decode("utf-8").splitlines(keepends=True), python_changed.decode("utf-8").splitlines(keepends=True), fromfile=str(python_file), tofile=str(python_file))), end="")
            if handoff_missing:
                print(f"Create {handoff}:\n{compaction_advisor.HANDOFF_TEMPLATE}", end="")
            if changed == original and scaffold is None and not python_update and not handoff_missing:
                print("No changes.")
            return 0
        if changed != original:
            if (config.read_bytes() if config.exists() else None) != original:
                raise ValueError("config changed during setup; run the plan again")
            if original is not None:
                stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
                backup = config.with_name(f"config.toml.bak-{stamp}")
                with backup.open("xb") as stream:
                    stream.write(original)
                    stream.flush()
                    os.fsync(stream.fileno())
            _atomic(config, changed)
        if scaffold is not None and not profile.exists():
            try:
                _atomic(profile, scaffold, exclusive=True)
            except FileExistsError:
                pass
        handoff_created = False
        if handoff_missing:
            try:
                _atomic(handoff, compaction_advisor.HANDOFF_TEMPLATE.encode("utf-8"), exclusive=True)
                handoff_created = True
            except FileExistsError:
                pass
        if python_update:
            if (python_file.read_bytes() if python_file.exists() else None) != python_original:
                raise ValueError("Python locator path changed during setup; run the plan again")
            _atomic(python_file, python_changed)
        print("Applied." if changed != original or scaffold is not None or python_update or handoff_created else "No changes.")
        print(f"Handoff: {handoff}")
        return 0
    except (OSError, UnicodeError, ValueError, tomllib.TOMLDecodeError) as exc:
        print(f"purser:init: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
