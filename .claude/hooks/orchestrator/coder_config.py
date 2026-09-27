"""User-selected coding agent and routes layered over shipped defaults."""

import json
import os
from pathlib import Path
import re
import shutil
import sys
import tomllib


MODEL = re.compile(r"[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\Z")
AGY_MODEL = re.compile(r"gemini-[A-Za-z0-9._-]+\Z")
AGY_EFFORTS = frozenset(("low", "medium", "high", "max"))
FAMILY = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")


def opencode_candidates() -> list[str]:
    """Return measured OpenCode candidates from the local inventory, without probing."""
    try:
        import state_root
        path = state_root.default_state_root() / "opencode" / "candidates.json"
        value = json.loads(path.read_text(encoding="utf-8"))
        candidates = value.get("candidates") if isinstance(value, dict) else None
        if not isinstance(candidates, list):
            return []
        return [item["id"] for item in candidates
                if isinstance(item, dict) and item.get("status") == "measured"
                and isinstance(item.get("id"), str) and MODEL.fullmatch(item["id"])]
    except (OSError, ValueError, TypeError, AttributeError):
        return []


def agy_binary() -> str | None:
    return shutil.which("agy")


def opencode_binary() -> str | None:
    candidates = opencode_binaries()
    return candidates[1] if len(candidates) > 1 else candidates[0] if candidates else None


def opencode_binaries() -> list[str]:
    launcher = shutil.which("opencode")
    if not launcher:
        return []
    candidates = [launcher]
    platform_packages = {
        "win32": ("opencode-windows-x64", "opencode-windows-x64-baseline"),
        "linux": ("opencode-linux-x64", "opencode-linux-arm64"),
        "darwin": ("opencode-darwin-x64", "opencode-darwin-arm64"),
    }
    launcher_path = Path(launcher)
    package_roots = [launcher_path.parent / "node_modules" / "opencode-ai" / "node_modules",
                     launcher_path.parent.parent / "node_modules" / "opencode-ai" / "node_modules",
                     launcher_path.parent.parent / "node_modules",
                     launcher_path.parent.parent / "opencode-ai" / "node_modules"]
    binary_name = "opencode.exe" if os.name == "nt" else "opencode"
    for package in dict.fromkeys(package_roots):
        for variant in platform_packages.get(sys.platform, ()):
            candidate = package / variant / "bin" / binary_name
            if candidate.is_file():
                candidates.append(str(candidate))
    if os.name == "nt" and Path(launcher).suffix.lower() != ".exe":
        candidates.remove(launcher)
    return list(dict.fromkeys(candidates))


def load(config_path: Path) -> dict:
    config = json.loads(config_path.read_text(encoding="utf-8"))
    user_path = Path.home() / ".orchestrator" / "config.toml"
    if not user_path.exists():
        return config
    with user_path.open("rb") as stream:
        user = tomllib.load(stream)
    mechanical = user.get("mechanical", {})
    if not isinstance(mechanical, dict) or type(mechanical.get("opencode_enabled", True)) is not bool:
        raise ValueError("[mechanical] opencode_enabled must be a boolean")
    config["mechanical_opencode_enabled"] = mechanical.get("opencode_enabled", True)
    coding = user.get("coding", {})
    if not isinstance(coding, dict):
        raise ValueError("[coding] must be a table")
    agent = coding.get("coder_agent", "codex")
    if agent not in ("codex", "opencode", "agy"):
        raise ValueError("[coding] coder_agent is invalid")
    family = coding.get("family")
    if family is not None:
        if agent not in ("opencode", "agy") or not isinstance(family, str) or not FAMILY.fullmatch(family):
            raise ValueError("[coding] family is invalid")
        config["coder_family"] = family
    role = user.get("roles", {}).get("coder", "codex")
    if agent in ("opencode", "agy"):
        routes = coding.get("routes")
        if role != agent or not isinstance(routes, dict) or set(routes) != set(config["required_routes"]):
            raise ValueError("[coding] coder routes or role are invalid")
        pattern = MODEL if agent == "opencode" else AGY_MODEL
        if agent == "agy":
            if not all(isinstance(route, dict) and set(route) == {"model", "effort"}
                       and isinstance(route["model"], str) and pattern.fullmatch(route["model"])
                       and isinstance(route["effort"], str) and route["effort"] in AGY_EFFORTS
                       for route in routes.values()):
                raise ValueError("[coding] Antigravity route model or effort is invalid")
            if len({route["model"] for route in routes.values()}) != 1:
                raise ValueError("[coding] coder routes must use one model")
            config["routes"] = routes
        else:
            if not all(isinstance(model, str) and pattern.fullmatch(model) for model in routes.values()):
                raise ValueError("[coding] coder route model is invalid")
            if len(set(routes.values())) != 1:
                raise ValueError("[coding] coder routes must use one model")
            config["routes"] = {name: {"model": model, "effort": "none"} for name, model in routes.items()}
    elif role in ("opencode", "agy"):
        raise ValueError("[coding] coder agent and routes disagree")
    else:
        routes = coding.get("routes", {})
        if not isinstance(routes, dict) or (routes and (role != "codex" or set(routes) != set(config["required_routes"]))):
            raise ValueError("[coding] coder routes or role are invalid")
        if routes:
            if not all(isinstance(entry, dict) and set(entry) == {"model", "effort"}
                       and isinstance(entry["model"], str) and re.fullmatch(r"[A-Za-z0-9._-]+", entry["model"])
                       and entry["effort"] in config["allowed_efforts"] for entry in routes.values()):
                raise ValueError("[coding] Codex route model or effort is invalid")
            config["routes"] = routes
    config["coder_agent"] = agent
    return config
