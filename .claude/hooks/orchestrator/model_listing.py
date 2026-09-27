"""Init-only, read-only model listings from coder CLIs."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
import json
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

import coder_config
import state_root


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


def _candidates_path(root=None) -> Path:
    return (Path(root) if root is not None else state_root.default_state_root()) / "opencode" / "candidates.json"


def stale(max_age_hours=24, *, root=None) -> bool:
    """Return whether the OpenCode candidate inventory is missing or old."""
    try:
        value = json.loads(_candidates_path(root).read_text(encoding="utf-8"))
        refreshed = datetime.fromisoformat(value["refreshed_at"].replace("Z", "+00:00"))
        if refreshed.tzinfo is None or not isinstance(value.get("candidates"), list):
            return True
        return datetime.now(timezone.utc) - refreshed > timedelta(hours=max_age_hours)
    except (OSError, ValueError, TypeError, KeyError, AttributeError, OverflowError):
        return True


def refresh_candidates(providers: dict, *, root=None) -> bool:
    """Refresh the local eligible-free OpenCode inventory; leave it intact on failure."""
    path = _candidates_path(root)
    try:
        listing = list_models("opencode", providers)
        if (listing.get("status") != "available" or not isinstance(listing.get("models"), list)
                or not listing["models"]):
            return False
        eligible = []
        for model in listing["models"]:
            model_id = model.get("id") if isinstance(model, dict) else None
            if not isinstance(model_id, str) or not coder_config.MODEL.fullmatch(model_id):
                return False
            provider, separator, bare_id = model_id.partition("/")
            fact = providers.get("opencode", {}).get(bare_id if separator and provider == "opencode" else model_id)
            cost = fact.get("cost") if isinstance(fact, dict) else None
            if model_id.endswith("-free") or (
                isinstance(cost, dict) and cost.get("input") == 0 and cost.get("output") == 0
            ):
                eligible.append(model_id)

        existing = {"refreshed_at": None, "candidates": []}
        try:
            existing = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(existing, dict) or not isinstance(existing.get("candidates"), list):
                return False
        except FileNotFoundError:
            pass
        old_by_id = {}
        for item in existing["candidates"]:
            if (not isinstance(item, dict) or not isinstance(item.get("id"), str)
                    or item.get("status") not in ("unmeasured", "measured", "failed")
                    or not isinstance(item.get("added_at"), str)):
                return False
            old_by_id[item["id"]] = item
        now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        candidates = [old_by_id[model_id] for model_id in old_by_id if model_id in eligible]
        candidates.extend({"id": model_id, "status": "unmeasured", "added_at": now}
                          for model_id in eligible if model_id not in old_by_id)
        _atomic_write(path, {"refreshed_at": now, "candidates": candidates})
        return True
    except Exception:
        return False


def _atomic_write(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=path.name + ".", suffix=".tmp", delete=False) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass


def mark_candidate_failed(model_id: str, *, root=None) -> bool:
    """Atomically retire one candidate while preserving the rest of the inventory."""
    path = _candidates_path(root)
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(value, dict) or not isinstance(value.get("candidates"), list):
            return False
        found = False
        for item in value["candidates"]:
            if not isinstance(item, dict) or not isinstance(item.get("id"), str):
                return False
            if item["id"] == model_id:
                item["status"] = "failed"
                found = True
        if not found:
            return False
        _atomic_write(path, value)
        return True
    except (OSError, ValueError, TypeError):
        return False
