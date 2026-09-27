"""User-selected coding agent and routes layered over shipped defaults."""

import json
import os
from pathlib import Path
import re
import shutil
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
    launcher = shutil.which("opencode")
    if not launcher:
        return None
    if os.name == "nt":
        package = Path(launcher).parent / "node_modules" / "opencode-ai" / "node_modules"
        for variant in ("opencode-windows-x64", "opencode-windows-x64-baseline"):
            candidate = package / variant / "bin" / "opencode.exe"
            if candidate.is_file():
                return str(candidate)
        return launcher if Path(launcher).suffix.lower() == ".exe" else None
    return launcher


def load(config_path: Path) -> dict:
    config = json.loads(config_path.read_text(encoding="utf-8"))
    user_path = Path.home() / ".orchestrator" / "config.toml"
    if not user_path.exists():
        return config
    with user_path.open("rb") as stream:
        user = tomllib.load(stream)
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
