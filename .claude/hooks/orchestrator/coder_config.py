"""User-selected coding agent and routes layered over shipped defaults."""

import json
import os
from pathlib import Path
import re
import shutil
import tomllib


MODEL = re.compile(r"[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\Z")


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
    if agent not in ("codex", "opencode"):
        raise ValueError("[coding] coder_agent is invalid")
    role = user.get("roles", {}).get("coder", "codex")
    if agent == "opencode":
        routes = coding.get("routes")
        if role != "opencode" or not isinstance(routes, dict) or set(routes) != set(config["required_routes"]):
            raise ValueError("[coding] OpenCode routes or role are invalid")
        if not all(isinstance(model, str) and MODEL.fullmatch(model) for model in routes.values()):
            raise ValueError("[coding] OpenCode route model is invalid")
        if len(set(routes.values())) != 1:
            raise ValueError("[coding] OpenCode routes must use one model")
        config["routes"] = {name: {"model": model, "effort": "none"} for name, model in routes.items()}
    elif role == "opencode" or coding.get("routes", {}) != {}:
        raise ValueError("[coding] coder agent and routes disagree")
    config["coder_agent"] = agent
    return config
