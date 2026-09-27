"""Init-only, read-only model listings from coder CLIs."""

from __future__ import annotations

import json
import re
import shutil
import subprocess

import coder_config


_EFFORTS = ("low", "medium", "high", "max")
_ID = re.compile(r"[A-Za-z0-9._/-]+\Z")


def _family(model_id: str, providers: dict) -> str | None:
    if "/" in model_id:
        provider, bare_id = model_id.split("/", 1)
        fact = providers.get(provider, {}).get(bare_id)
    else:
        matches = [models[model_id] for models in providers.values() if model_id in models]
        fact = matches[0] if len(matches) == 1 else None
    return fact.get("family") if isinstance(fact, dict) else None


def parse_codex(output: str) -> list[dict]:
    data = json.loads(output)
    if not isinstance(data, dict) or not isinstance(data.get("models"), list):
        raise ValueError("invalid codex model listing")
    models = []
    for item in data["models"]:
        if not isinstance(item, dict) or item.get("visibility") != "list":
            continue
        slug, levels = item.get("slug"), item.get("supported_reasoning_levels")
        if not isinstance(slug, str) or not _ID.fullmatch(slug) or not isinstance(levels, list):
            raise ValueError("invalid visible Codex model")
        efforts = [level.get("effort") for level in levels if isinstance(level, dict)]
        if len(efforts) != len(levels) or not efforts or any(not isinstance(e, str) or not e for e in efforts):
            raise ValueError("invalid Codex effort levels")
        models.append({"id": slug, "efforts": efforts})
    if not models:
        raise ValueError("no visible Codex models")
    return models


def parse_opencode(output: str) -> list[dict]:
    ids = [line.strip() for line in output.splitlines() if coder_config.MODEL.fullmatch(line.strip())]
    if not ids:
        raise ValueError("invalid OpenCode model listing")
    return [{"id": model_id, "efforts": None} for model_id in dict.fromkeys(ids)]


def parse_agy(output: str) -> list[dict]:
    grouped: dict[str, dict] = {}
    for line in output.splitlines():
        if not line.strip():
            continue
        if "\t" not in line:
            continue
        raw_id, label = line.split("\t", 1)
        if not label.strip() or not _ID.fullmatch(raw_id):
            continue
        base, separator, suffix = raw_id.rpartition("-")
        effort = suffix if separator and suffix in _EFFORTS else None
        model_id = base if effort else raw_id
        entry = grouped.setdefault(model_id, {"id": model_id, "efforts": [], "variants": {}})
        if effort:
            entry["efforts"].append(effort)
            entry["variants"][effort] = raw_id
        else:
            entry["efforts"] = None
            entry["variants"]["default"] = raw_id
    if not grouped:
        raise ValueError("no agy models")
    return list(grouped.values())


def list_models(agent: str, providers: dict) -> dict:
    """Return verified CLI choices; a failed listing never falls back to catalog IDs."""
    if agent == "codex":
        binary, args, parser = shutil.which("codex"), ("debug", "models"), parse_codex
    elif agent == "opencode":
        binary, args, parser = coder_config.opencode_binary(), ("models",), parse_opencode
    elif agent == "agy":
        binary, args, parser = coder_config.agy_binary(), ("models",), parse_agy
    else:
        raise ValueError("unsupported coder agent")
    source = f"{agent} {' '.join(args)}"
    if not binary:
        return {"status": "unavailable", "models": [], "source": source, "reason": "binary not found"}
    try:
        result = subprocess.run([binary, *args], capture_output=True, text=True, encoding="utf-8",
                                errors="replace", timeout=30, check=False)
        if result.returncode:
            raise ValueError(f"listing exited {result.returncode}")
        models = parser(result.stdout)
    except (OSError, UnicodeError, ValueError, subprocess.TimeoutExpired) as exc:
        reason = "listing timed out" if isinstance(exc, subprocess.TimeoutExpired) else str(exc)
        return {"status": "unavailable", "models": [], "source": source, "reason": reason}
    for model in models:
        model["family"] = _family(model["id"], providers)
        model["source"] = source
    return {"status": "available", "models": models, "source": source}
