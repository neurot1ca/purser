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
import coder_config
import model_catalog
import model_listing
import model_version_notice
import mechanical_learning
import policy


HEAD = re.compile(r"^\s*\[([^\[\]]+)\]\s*(?:#.*)?$")
KEY = re.compile(r"^\s*([A-Za-z0-9_-]+)\s*=")
# Ugur 2026-09-27: reviewed Maximum model policy; listings supply the effort.
MAXIMUM_MODELS = {"codex": "gpt-6-astra"}


def _mode_routes(mode: str, coder: str, model: str | None, effort: str | None) -> tuple[dict, str | None]:
    shipped = json.loads(Path(__file__).with_name("config.json").read_text(encoding="utf-8"))
    if mode == "balanced":
        if coder != "codex":
            raise ValueError("balanced mode has no reviewed policy for this coder")
        return deepcopy(shipped["routes"]), None
    if mode == "maximum":
        model = MAXIMUM_MODELS.get(coder)
        if model is None:
            raise ValueError(f"maximum mode has no reviewed model policy for {coder}")
    catalog = model_catalog.load_catalog()
    listing = model_listing.list_models(coder, catalog["providers"])
    if listing["status"] != "available":
        raise ValueError(f"{coder} model listing unavailable: {listing.get('reason', 'unknown reason')}")
    choices = {entry["id"]: entry for entry in listing["models"]}
    if not isinstance(model, str) or model not in choices:
        raise ValueError(f"model {model!r} is absent from the live {coder} listing")
    levels = choices[model]["efforts"]
    family = _family(choices[model]) if coder in ("opencode", "agy") else None
    if mode == "maximum":
        if not levels:
            raise ValueError(f"{model} has no listed effort levels")
        try:
            effort = max(levels, key=shipped["allowed_efforts"].index)
        except ValueError as exc:
            raise ValueError(f"{model} lists an unsupported effort level") from exc
    elif (levels is None and effort is not None) or (levels is not None and effort not in levels):
        raise ValueError(f"effort {effort!r} is absent from the live listing for {model}")
    if coder == "codex":
        if not re.fullmatch(r"[A-Za-z0-9._-]+", model) or effort not in shipped["allowed_efforts"]:
            raise ValueError("listed Codex model or effort is invalid for Purser routes")
    elif coder == "opencode":
        if not coder_config.MODEL.fullmatch(model) or effort is not None:
            raise ValueError("listed OpenCode model or effort is invalid for Purser routes")
    elif coder == "agy":
        if not coder_config.AGY_MODEL.fullmatch(model) or effort not in coder_config.AGY_EFFORTS:
            raise ValueError("listed Antigravity model or effort is invalid for Purser routes")
    pair = {"model": model, "effort": effort} if effort is not None else model
    return {route: deepcopy(pair) for route in shipped["required_routes"]}, family


def _family(choice: dict) -> str | None:
    family = choice.get("family")
    if family is not None and (not isinstance(family, str) or not coder_config.FAMILY.fullmatch(family)):
        raise ValueError("listed model family is invalid")
    return family


def _listed_family(coder: str, model: str) -> str | None:
    catalog = model_catalog.load_catalog()
    listing = model_listing.list_models(coder, catalog["providers"])
    if listing["status"] != "available":
        raise ValueError(f"{coder} model listing unavailable: {listing.get('reason', 'unknown reason')}")
    choice = next((entry for entry in listing["models"] if entry["id"] == model), None)
    if choice is None:
        raise ValueError(f"model {model!r} is absent from the live {coder} listing")
    return _family(choice)


def _validate_fallback(agent: str, model: str, primary_model: str, config_path: Path,
                       project: Path) -> dict:
    if model == primary_model:
        raise ValueError("fallback model must differ from the primary coder model")
    detected = detect(config_path, project)
    report = detected["agents"].get(agent, {})
    available_models = {entry["id"] if isinstance(entry, dict) else entry
                        for entry in report.get("models", [])}
    if not report.get("available") or model not in available_models:
        raise ValueError("fallback coder or model is not detected as working")
    family = _listed_family(agent, model)
    if family is None:
        raise ValueError("fallback model family is unavailable")
    return {"agent": agent, "model": model, "family": family}


def _agent(name: str, executable: str | None = None) -> dict:
    path = executable or shutil.which(name)
    if not path:
        return {"available": False, "path": None, "version": None}
    path = str(Path(path).expanduser())
    if not Path(path).is_file() and shutil.which(path) is None:
        return {"available": False, "path": path, "version": None}
    version = None
    try:
        result = subprocess.run([path, "--version"], capture_output=True, text=True, timeout=5, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
        if result.returncode == 0:
            version = (result.stdout or result.stderr).strip() or None
    except (OSError, subprocess.TimeoutExpired):
        pass
    return {"available": True, "path": path, "version": version}


def detect(config_path: Path, project: Path, coder: str | None = None, model: str | None = None) -> dict:
    data = tomllib.loads(config_path.read_text(encoding="utf-8")) if config_path.exists() else {}
    reviewer = data.get("reviewer", {})
    configured = reviewer.get("executable") if isinstance(reviewer, dict) else None
    trust = data.get("trust", {})
    roles = data.get("roles", {})
    agy = _agent("agy", configured if isinstance(configured, str) else coder_config.agy_binary())
    if not agy["available"] and configured:
        agy = _agent("agy")
    agy["models"] = []
    if agy["available"]:
        try:
            agy["models"] = _agy_models(agy["path"])
        except (OSError, UnicodeError, ValueError):
            pass
    if agy["models"]:
        agy["options"] = _model_options("agy", agy["models"])
    opencode = {"available": False, "path": None, "version": None, "models": [],
                "reason": "OpenCode command and platform binary failed"}
    working = _working_opencode()
    if working:
        binary, models = working
        opencode = _agent("opencode", binary)
        opencode["models"] = models
        opencode["options"] = _model_options("opencode", models)
        try:
            _refresh_opencode_candidates(models, model_catalog.load_cached_catalog()["providers"])
        except (OSError, UnicodeError, ValueError, KeyError, TypeError):
            pass
    codex = _agent("codex")
    codex["models"] = []
    codex["options"] = []
    if codex["available"]:
        try:
            listing = model_listing.list_models("codex", model_catalog.load_cached_catalog()["providers"])
            if listing.get("status") == "available":
                codex["models"] = [entry["id"] for entry in listing["models"]]
                codex["options"] = _model_options("codex", listing["models"])
        except (OSError, UnicodeError, ValueError, KeyError, TypeError):
            pass
    reviewer_options = []
    for agent, report in (("codex", codex), ("agy", agy), ("opencode", opencode)):
        if report.get("available"):
            reviewer_options.extend(_reviewer_options(agent, report.get("models", [])))
    coder_family = _selected_coder_family(data, coder, model)
    if coder_family is not None:
        reviewer_options = _mark_reviewer_options(reviewer_options, coder_family)
    return {
        "agents": {
            "codex": codex,
            "agy": agy,
            "claude": _agent("claude"),
            "opencode": opencode,
        },
        "reviewer_options": reviewer_options,
        "coder_family": coder_family,
        "roles": roles,
        "trusted": str(project) in trust.get("paths", []) if isinstance(trust, dict) else False,
        "profile_exists": (project / ".orchestrator" / "profile.toml").exists(),
    }


def _refresh_opencode_candidates(models: list[str], providers: dict) -> None:
    model_listing.refresh_candidates(providers, model_ids=models)
    try:
        mechanical_learning.start_benchmark()
    except Exception:
        pass


def _opencode_models(binary: str) -> list[str]:
    try:
        result = subprocess.run([binary, "models"], capture_output=True, text=True, timeout=30, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
    except subprocess.TimeoutExpired as exc:
        raise ValueError("opencode models timed out") from exc
    if result.returncode:
        raise ValueError("opencode models failed")
    models = [line.strip() for line in result.stdout.splitlines()
              if coder_config.MODEL.fullmatch(line.strip())]
    if not models:
        raise ValueError("opencode models returned an invalid list")
    return models


def _working_opencode(model: str | None = None) -> tuple[str, list[str]] | None:
    for binary in coder_config.opencode_binaries():
        try:
            models = _opencode_models(binary)
        except (OSError, UnicodeError, ValueError):
            continue
        if model is None or model in models:
            return binary, models
    return None


def _model_options(agent: str, models: list[dict]) -> list[dict]:
    entries = []
    for model in models:
        entry = {"id": model, "family": policy.model_family(model)} if isinstance(model, str) else dict(model)
        if agent == "opencode" and isinstance(entry.get("id"), str) and (
                entry["id"] == "opencode/big-pickle" or entry["id"].startswith("opencode/") and entry["id"].endswith("-free")):
            entry["family"] = "opencode"
        entries.append(entry)
    free = [entry for entry in entries if agent == "opencode" and (
        entry.get("id") == "opencode/big-pickle" or
        (isinstance(entry.get("id"), str) and entry["id"].startswith("opencode/") and entry["id"].endswith("-free")))]
    options = []
    if free:
        families = {policy.vendor_family(entry.get("family")) for entry in free}
        options.append({"label": "Free OpenCode models (Purser races them)",
                        "models": [entry["id"] for entry in free],
                        "family": next(iter(families)) if len(families) == 1 else None})
    options.extend({"label": entry["id"], "models": [entry["id"]],
                    "family": policy.vendor_family(entry.get("family"))}
                   for entry in entries if entry not in free)
    return options


def _reviewer_options(agent: str, models: list) -> list[dict]:
    options = []
    for entry in models:
        model = entry if isinstance(entry, str) else entry.get("id")
        if not isinstance(model, str):
            continue
        family = entry.get("family") if isinstance(entry, dict) else policy.model_family(model)
        if agent == "opencode" and (model == "opencode/big-pickle" or
                                     model.startswith("opencode/") and model.endswith("-free")):
            family = "opencode"
        options.append({"agent": agent, "model": model, "label": model,
                        "family": policy.vendor_family(family)})
    return options


def _selected_coder_family(data: dict, coder: str | None, model: str | None) -> str | None:
    roles = data.get("roles", {})
    coding = data.get("coding", {})
    selected_coder = coder or (roles.get("coder") if isinstance(roles, dict) else None)
    if selected_coder == "claude":
        return "anthropic"
    if selected_coder not in ("codex", "opencode", "agy"):
        return None
    selected_model = model
    configured_coder = roles.get("coder") if isinstance(roles, dict) else None
    if selected_model is None and configured_coder == selected_coder and isinstance(coding, dict):
        routes = coding.get("routes", {})
        route = routes.get("normal") if isinstance(routes, dict) else None
        selected_model = route.get("model") if isinstance(route, dict) else route
    if selected_model is None and selected_coder == "codex":
        shipped = json.loads(Path(__file__).with_name("config.json").read_text(encoding="utf-8"))
        selected_model = shipped["routes"]["normal"]["model"]
    family = (coding.get("family") if model is None and configured_coder == selected_coder
              and isinstance(coding, dict) else None)
    if family is None and selected_model is not None:
        options = _model_options(selected_coder, [selected_model])
        family = options[0].get("family") if options else None
    return policy.vendor_family(family)


def _same_vendor(option: dict, coder_family: str | None) -> bool:
    family = policy.vendor_family(coder_family)
    return family is not None and option.get("family") == family


def _mark_reviewer_options(options: list[dict], coder_family: str | None) -> list[dict]:
    return [{**option, "same_family": _same_vendor(option, coder_family)} for option in options]


def _reviewer_executable(agent: str, model: str) -> str | None:
    if agent == "agy":
        binary = coder_config.agy_binary()
        if not binary or model not in _agy_models(binary):
            raise ValueError("selected Reviewer-1 model is absent from agy models")
        return binary
    if agent == "opencode":
        working = _working_opencode(model)
        if not working:
            raise ValueError("selected Reviewer-1 model is absent from opencode models")
        return working[0]
    if agent == "codex":
        binary = shutil.which("codex")
        listing = model_listing.list_models("codex", model_catalog.load_cached_catalog()["providers"])
        if not binary or listing.get("status") != "available" or model not in {entry["id"] for entry in listing["models"]}:
            raise ValueError("selected Reviewer-1 model is absent from codex models")
        return binary
    return None


def _agy_models(binary: str) -> list[str]:
    try:
        result = subprocess.run([binary, "models"], capture_output=True, text=True, timeout=30, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
    except subprocess.TimeoutExpired as exc:
        raise ValueError("agy models timed out") from exc
    if result.returncode:
        raise ValueError("agy models failed")
    models = [line.split("\t", 1)[0] for line in result.stdout.splitlines()
              if "\t" in line and coder_config.AGY_MODEL.fullmatch(line.split("\t", 1)[0])]
    if not models:
        raise ValueError("agy models returned no Gemini models")
    return models


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


def _candidate(original: bytes | None, coder: str, reviewer: str, trust: bool, project: Path,
               model: str | None = None, routes: dict | None = None, family: str | None = None,
               reviewer_model: str | None = None,
               reviewer_executable: str | None = None,
               opencode_mechanical: bool | None = None,
               fallback: dict | None = None) -> bytes:
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
    if reviewer != "none" and reviewer_model is not None:
        selected = after.setdefault("reviewer", {})
        if not isinstance(selected, dict):
            raise ValueError("[reviewer] must be a table")
        selected["selected"] = {"agent": reviewer, "model": reviewer_model}
        text = _upsert(text, "reviewer", "selected", {"agent": reviewer, "model": reviewer_model}, newline,
                       rendered=f"selected = {{ agent = {json.dumps(reviewer)}, model = {json.dumps(reviewer_model)} }}{newline}")
    if reviewer_executable is not None:
        reviewer_table = after.setdefault("reviewer", {})
        if not isinstance(reviewer_table, dict):
            raise ValueError("[reviewer] must be a table")
        reviewer_table["executable"] = reviewer_executable
        text = _upsert(text, "reviewer", "executable", reviewer_executable, newline)
    if opencode_mechanical is not None:
        mechanical = after.setdefault("mechanical", {})
        if not isinstance(mechanical, dict):
            raise ValueError("[mechanical] must be a table")
        mechanical["opencode_enabled"] = opencode_mechanical
        text = _upsert(text, "mechanical", "opencode_enabled", opencode_mechanical, newline)
    if fallback is not None:
        after["fallback"] = fallback
        text = _upsert(text, "fallback", "agent", fallback["agent"], newline)
        text = _upsert(text, "fallback", "model", fallback["model"], newline)
        text = _upsert(text, "fallback", "family", fallback["family"], newline)
    if routes is not None or coder in ("opencode", "agy") or "coding" in after:
        coding = after.setdefault("coding", {})
        if not isinstance(coding, dict):
            raise ValueError("[coding] must be a table")
        had_family = "family" in coding
        coding["coder_agent"] = coder if coder in ("opencode", "agy") else "codex"
        coding["routes"] = routes if routes is not None else ({name: ({"model": model, "effort": "medium"} if coder == "agy" else model)
                             for name in json.loads(
            (Path(__file__).with_name("config.json")).read_text(encoding="utf-8"))["required_routes"]}
            if coder in ("opencode", "agy") else {})
        if coder in ("opencode", "agy") and family is not None:
            coding["family"] = family
        else:
            coding.pop("family", None)
        text = _upsert(text, "coding", "coder_agent", coding["coder_agent"], newline)
        if coder in ("opencode", "agy") and family is not None:
            text = _upsert(text, "coding", "family", family, newline)
        elif had_family:
            lines = text.splitlines(keepends=True)
            span = _span(lines, "coding")
            if span:
                start, end = span
                lines = [line for index, line in enumerate(lines)
                         if not (start < index < end and (match := KEY.match(line)) and match.group(1) == "family")]
                text = "".join(lines)
        routes = coding["routes"]
        rendered = "routes = {" + ", ".join(
            f"{name} = {{ model = {json.dumps(value['model'])}, effort = {json.dumps(value['effort'])} }}"
            if isinstance(value, dict) else f"{name} = {json.dumps(value)}"
            for name, value in routes.items()) + "}" + newline
        text = _upsert(text, "coding", "routes", routes, newline, rendered=rendered)
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


def _model_version_decision(version: str, *, adopt: bool) -> None:
    if not re.fullmatch(r"[A-Za-z0-9._/-]+", version):
        raise ValueError("model version is invalid")
    if not adopt:
        model_version_notice.decide(version, decline=True)
        print(f"Declined model version {version}.")
        return
    config_path = Path.home() / ".orchestrator" / "config.toml"
    original = config_path.read_bytes() if config_path.exists() else None
    before = tomllib.loads(original.decode("utf-8")) if original is not None else {}
    loaded = coder_config.load(Path(__file__).with_name("config.json"))
    agent = loaded.get("coder_agent", "codex")
    catalog = model_catalog.load_cached_catalog()
    listing = model_listing.list_models(agent, catalog["providers"])
    if listing.get("status") != "available" or version not in {
            item.get("id") for item in listing.get("models", []) if isinstance(item, dict)}:
        raise ValueError(f"model {version!r} is absent from the live {agent} listing")
    routes = before.get("coding", {}).get("routes", {}) if isinstance(before.get("coding", {}), dict) else {}
    if not routes:
        routes = {name: route for name, route in json.loads(
            Path(__file__).with_name("config.json").read_text(encoding="utf-8"))["routes"].items()}
    updated = deepcopy(routes)
    changed = False
    for name, route in updated.items():
        current = route.get("model") if isinstance(route, dict) else route
        if isinstance(current, str) and model_version_notice.is_newer(current, version):
            if isinstance(route, dict):
                route["model"] = version
            else:
                updated[name] = version
            changed = True
    if not changed:
        raise ValueError(f"no pinned route has a strictly older version in {version!r}'s model family")
    if original is None:
        text = "schema_version = 1\n"
        before = {"schema_version": 1}
    else:
        text = original.decode("utf-8")
    newline = "\r\n" if "\r\n" in text else "\n"
    coding = before.get("coding", {})
    if not isinstance(coding, dict):
        raise ValueError("[coding] must be a table")
    if "coder_agent" not in coding:
        text = _upsert(text, "coding", "coder_agent", agent, newline)
    rendered = "routes = {" + ", ".join(
        f"{name} = {{ model = {json.dumps(value['model'])}, effort = {json.dumps(value['effort'])} }}"
        if isinstance(value, dict) else f"{name} = {json.dumps(value)}"
        for name, value in updated.items()) + "}" + newline
    text = _upsert(text, "coding", "routes", updated, newline, rendered=rendered)
    after = tomllib.loads(text)
    expected = deepcopy(before)
    expected_coding = expected.setdefault("coding", {})
    expected_coding["coder_agent"] = agent
    expected_coding["routes"] = updated
    if after != expected:
        raise ValueError("model adoption changed config values outside coding routes")
    if (config_path.read_bytes() if config_path.exists() else None) != original:
        raise ValueError("config changed during model adoption")
    if original is not None:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        backup = config_path.with_name(f"config.toml.bak-{stamp}")
        with backup.open("xb") as stream:
            stream.write(original)
            stream.flush()
            os.fsync(stream.fileno())
    _atomic(config_path, text.encode("utf-8"))
    model_version_notice.decide(version, decline=False)
    print(f"Adopted model version {version} for its pinned routes.")


def _output_style(path: Path) -> tuple[bool, object]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return False, None
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return "outputStyle" in data, data.get("outputStyle")


def _effective_output_style(project: Path) -> tuple[object, bool]:
    """Return the winning style and whether a project setting overrides the user setting."""
    claude = project / ".claude"
    for path in (claude / "settings.local.json", claude / "settings.json"):
        try:
            present, style = _output_style(path)
        except (OSError, UnicodeError, ValueError):
            return None, False
        if present:
            return style, True
    try:
        _, style = _output_style(Path.home() / ".claude" / "settings.json")
    except (OSError, UnicodeError, ValueError):
        return None, False
    return style, False


def _claim_concise_style_offer(project: Path) -> bool:
    try:
        style, _ = _effective_output_style(project)
        if style == "Concise":
            return False
        marker = Path.home() / ".orchestrator" / "reply-style-suggestion-shown"
        marker.parent.mkdir(parents=True, exist_ok=True)
        with marker.open("x", encoding="utf-8"):
            pass
        return True
    except FileExistsError:
        return False
    except OSError:
        return False


def _enable_concise_style(project: Path) -> bool:
    path = Path.home() / ".claude" / "settings.json"
    original = path.read_bytes() if path.exists() else None
    settings = json.loads(original.decode("utf-8")) if original is not None else {}
    if not isinstance(settings, dict):
        raise ValueError(f"{path} must contain a JSON object")
    settings["outputStyle"] = "Concise"
    if (path.read_bytes() if path.exists() else None) != original:
        raise ValueError("Claude settings changed during the update; try again")
    _atomic(path, (json.dumps(settings, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
    _, project_override = _effective_output_style(project)
    return project_override


def _python_to_store() -> tuple[Path, bool]:
    """Return the usable Python 3.11+ path and whether the active Python is a venv."""
    active = Path(sys.executable).resolve()
    in_venv = sys.prefix != sys.base_prefix
    if not in_venv:
        return active, False

    candidates = []
    base = Path(sys.base_prefix)
    if sys.platform == "win32":
        candidates.append(base / "python.exe")
    else:
        candidates.extend((base / "bin" / "python3", base / "bin" / "python"))
    base_executable = getattr(sys, "_base_executable", None)
    if base_executable:
        candidates.append(Path(base_executable))

    seen = set()
    for candidate in candidates:
        try:
            resolved = candidate.resolve()
            key = os.path.normcase(str(resolved))
            if key in seen or not resolved.is_file():
                continue
            seen.add(key)
            result = subprocess.run([str(resolved), "--version"], capture_output=True,
                                    text=True, timeout=5, check=False,
                                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000)
                                    if os.name == "nt" else 0)
            version = (result.stdout or result.stderr).strip()
            match = re.search(r"Python\s+(\d+)\.(\d+)", version)
            if result.returncode == 0 and match and (int(match.group(1)), int(match.group(2))) >= (3, 11):
                return resolved, True
        except (OSError, subprocess.TimeoutExpired, RuntimeError, ValueError):
            continue
    return active, True


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--detect", action="store_true")
    modes.add_argument("--list-models", action="store_true")
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--apply", action="store_true")
    modes.add_argument("--enable-concise-style", action="store_true")
    modes.add_argument("--adopt-model-version")
    modes.add_argument("--decline-model-version")
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--coder", choices=("codex", "claude", "opencode", "agy"))
    parser.add_argument("--model")
    parser.add_argument("--fallback-agent", choices=("codex", "opencode", "agy"))
    parser.add_argument("--fallback-model")
    parser.add_argument("--mode", choices=("balanced", "maximum", "manual"))
    parser.add_argument("--effort")
    parser.add_argument("--reviewer", choices=("agy", "opencode", "codex", "none"))
    parser.add_argument("--reviewer-model")
    parser.add_argument("--opencode-mechanical", action="store_const", const=True, default=None)
    parser.add_argument("--no-opencode-mechanical", action="store_const", const=False, dest="opencode_mechanical")
    parser.add_argument("--trust", action="store_true")
    parser.add_argument("--scaffold-profile", action="store_true")
    args = parser.parse_args(argv)
    if args.adopt_model_version or args.decline_model_version:
        try:
            _model_version_decision(args.adopt_model_version or args.decline_model_version,
                                    adopt=bool(args.adopt_model_version))
            return 0
        except (OSError, UnicodeError, ValueError, KeyError, TypeError, tomllib.TOMLDecodeError) as exc:
            print(f"purser:init: {exc}", file=sys.stderr)
            return 1
    if args.enable_concise_style:
        if (args.json or args.coder or args.model or args.fallback_agent or args.fallback_model or args.mode or args.effort
                or args.reviewer or args.reviewer_model or args.opencode_mechanical is not None or args.trust
                or args.scaffold_profile):
            parser.error("--enable-concise-style accepts no other options")
        try:
            project_override = _enable_concise_style(Path.cwd().resolve())
            print("Enabled Claude Code Concise reply style.")
            if project_override:
                print("A project-level outputStyle override still takes precedence for this project.")
            return 0
        except (OSError, UnicodeError, ValueError, json.JSONDecodeError) as exc:
            print(f"purser:init: {exc}", file=sys.stderr)
            return 1
    if args.detect:
        if not args.json or args.mode or args.effort or args.reviewer or args.reviewer_model or args.fallback_agent or args.fallback_model or args.opencode_mechanical is not None or args.trust or args.scaffold_profile:
            parser.error("--detect requires --json and accepts only optional --coder and --model")
        if args.model and not args.coder:
            parser.error("--detect --model requires --coder")
    elif args.list_models:
        if not args.json or args.coder not in ("codex", "opencode", "agy") or args.model or args.mode or args.effort or args.reviewer or args.reviewer_model or args.fallback_agent or args.fallback_model or args.opencode_mechanical is not None or args.trust or args.scaffold_profile:
            parser.error("--list-models requires --json and --coder codex, opencode, or agy")
    elif (args.json and not args.apply) or args.coder is None or args.reviewer is None:
        parser.error("--plan requires --coder and --reviewer without --json; --apply requires --coder and --reviewer")
    if bool(args.fallback_agent) != bool(args.fallback_model):
        parser.error("--fallback-agent and --fallback-model must be used together")
    if args.fallback_agent and args.coder == "claude":
        parser.error("fallback selection is not available when Claude is the primary coder")
    if args.mode == "manual" and not args.model:
        parser.error("--mode manual requires --model")
    if args.reviewer_model and args.reviewer == "none":
        parser.error("--reviewer-model cannot be used with --reviewer none")
    if args.reviewer in ("opencode", "codex") and not args.reviewer_model:
        parser.error(f"--reviewer {args.reviewer} requires --reviewer-model")
    if args.reviewer_model and args.reviewer is None:
        parser.error("--reviewer-model requires --reviewer")
    if args.mode in ("balanced", "maximum") and (args.model is not None or args.effort is not None):
        parser.error("--model and --effort require --mode manual")
    if not args.mode and args.effort is not None:
        parser.error("--effort requires --mode manual")
    if not args.mode and args.coder not in ("opencode", "agy") and args.model is not None:
        parser.error("--model requires --coder opencode or agy")
    project = Path.cwd().resolve()
    config = Path.home() / ".orchestrator" / "config.toml"
    python_file = config.with_name("python")
    profile = project / ".orchestrator" / "profile.toml"
    try:
        if args.detect:
            print(json.dumps(detect(config, project, args.coder, args.model), indent=2))
            return 0
        if args.list_models:
            catalog = model_catalog.load_catalog()
            listing = model_listing.list_models(args.coder, catalog["providers"])
            if args.coder == "opencode" and listing.get("status") == "available":
                _refresh_opencode_candidates([entry["id"] for entry in listing["models"]
                                              if isinstance(entry, dict) and isinstance(entry.get("id"), str)],
                                            catalog["providers"])
            print(json.dumps(listing, indent=2))
            return 0
        family = None
        if args.mode:
            routes, family = _mode_routes(args.mode, args.coder, args.model, args.effort)
        else:
            routes = None
        if routes is not None:
            shipped = json.loads(Path(__file__).with_name("config.json").read_text(encoding="utf-8"))
            shipped["coder_agent"] = args.coder
            shipped["routes"] = {name: value if isinstance(value, dict) else {"model": value, "effort": "none"}
                                 for name, value in routes.items()}
            policy.validate_config(shipped)
        if args.coder == "opencode" and not args.mode:
            working = _working_opencode(args.model)
            if not working:
                raise ValueError("OpenCode is unavailable or selected model is absent from opencode models")
            try:
                _refresh_opencode_candidates(
                    working[1], model_catalog.load_cached_catalog()["providers"]
                )
            except (OSError, UnicodeError, ValueError, KeyError, TypeError):
                pass
        if args.coder == "agy" and not args.mode:
            binary = coder_config.agy_binary()
            if not binary or not args.model or args.model not in _agy_models(binary):
                raise ValueError("Antigravity is unavailable or selected model is absent from agy models")
        if args.coder in ("opencode", "agy") and not args.mode:
            family = _listed_family(args.coder, args.model)
        fallback = None
        if args.fallback_agent:
            if args.coder in ("agy", "opencode"):
                primary_model = args.model
            elif args.mode:
                primary_model = routes["normal"]["model"] if isinstance(routes["normal"], dict) else routes["normal"]
            else:
                primary_model = json.loads(Path(__file__).with_name("config.json").read_text(encoding="utf-8"))["routes"]["normal"]["model"]
            fallback = _validate_fallback(args.fallback_agent, args.fallback_model, primary_model,
                                          config, project)
        handoff = compaction_advisor.handoff_path(project)
        original = config.read_bytes() if config.exists() else None
        reviewer_executable = None
        if args.reviewer_model:
            reviewer_executable = _reviewer_executable(args.reviewer, args.reviewer_model)
            if not reviewer_executable:
                raise ValueError(f"Reviewer-1 executable is unavailable for {args.reviewer}")
        changed = _candidate(original, args.coder, args.reviewer, args.trust, project, args.model, routes, family,
                             args.reviewer_model, reviewer_executable, args.opencode_mechanical, fallback)
        scaffold = _profile(project) if args.scaffold_profile and not profile.exists() else None
        handoff_missing = not handoff.exists()
        python_original = python_file.read_bytes() if python_file.exists() else None
        python_path, in_venv = _python_to_store()
        python_changed = (str(python_path) + "\n").encode("utf-8") if sys.version_info >= (3, 11) else None
        python_update = python_changed is not None and python_original != python_changed
        if args.plan:
            planned_routes = routes
            if planned_routes is None and args.coder in ("opencode", "agy"):
                planned_routes = tomllib.loads(changed.decode("utf-8")).get("coding", {}).get("routes")
            if planned_routes is not None:
                print("Resolved routes:")
                for name, value in planned_routes.items():
                    model = value["model"] if isinstance(value, dict) else value
                    effort = value.get("effort", "none") if isinstance(value, dict) else "none"
                    print(f"  {name} -> {model} / {effort}")
            if python_changed is not None:
                print(f"Python locator: {python_path}")
            if in_venv and python_path == Path(sys.executable).resolve():
                print(f"Note: this Python is inside a virtual environment ({sys.executable}); if it is removed, Purser hooks stop working.")
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
        offer_due = _claim_concise_style_offer(project)
        if args.json:
            print(json.dumps({"status": "applied", "handoff": str(handoff),
                              "concise_style_offer_due": offer_due}, indent=2))
        else:
            print("Applied." if changed != original or scaffold is not None or python_update or handoff_created else "No changes.")
            print(f"Handoff: {handoff}")
            if offer_due:
                print("Concise reply style offer due.")
        return 0
    except (OSError, UnicodeError, ValueError, tomllib.TOMLDecodeError) as exc:
        print(f"purser:init: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
