"""Bounded, init-time model facts from models.dev with offline fallbacks."""

from __future__ import annotations

from datetime import datetime, timezone
from http.client import HTTPException
import json
import os
from pathlib import Path
import tempfile
from typing import Any
from urllib.request import Request, urlopen

import state_root


URL = "https://models.dev/api.json"
TIMEOUT_SECONDS = 10
# The live endpoint is a few megabytes; leave room for catalog growth.
MAX_RESPONSE_BYTES = 16 * 1024 * 1024
_BOOTSTRAP_PATH = Path(__file__).with_name("model_catalog_bootstrap.json")


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _validate_catalog(value: Any) -> tuple[dict[str, dict[str, dict[str, Any]]], int]:
    if not isinstance(value, dict):
        raise ValueError("catalog must be a provider object")
    providers: dict[str, dict[str, dict[str, Any]]] = {}
    dropped_count = 0
    for provider_id, provider in value.items():
        if not isinstance(provider_id, str) or not provider_id or not isinstance(provider, dict):
            dropped_count += 1
            continue
        models = provider.get("models")
        if not isinstance(models, dict):
            dropped_count += 1
            continue
        checked: dict[str, dict[str, Any]] = {}
        for model_id, model in models.items():
            try:
                if not isinstance(model_id, str) or not model_id or not isinstance(model, dict):
                    raise ValueError("invalid model")
                name = model.get("name")
                reasoning = model.get("reasoning")
                if not isinstance(name, str) or not name or not isinstance(reasoning, bool):
                    raise ValueError("invalid name or reasoning")
                fact: dict[str, Any] = {"id": model_id, "name": name, "reasoning": reasoning}
                if "family" in model:
                    family = model["family"]
                    if not isinstance(family, str) or not family:
                        raise ValueError("invalid family")
                    fact["family"] = family
                # The API uses "limit"; the normalized cache uses "limits".
                limits = model.get("limit", model.get("limits", {}))
                if not isinstance(limits, dict):
                    raise ValueError("invalid limits")
                fact["limits"] = {}
                for key in ("context", "output"):
                    if key in limits:
                        count = limits[key]
                        if isinstance(count, bool) or not isinstance(count, int) or count <= 0:
                            raise ValueError(f"invalid {key} limit")
                        fact["limits"][key] = count
            except ValueError:
                dropped_count += 1
                continue
            checked[model_id] = fact
        providers[provider_id] = checked
    if not any(providers.values()):
        raise ValueError("catalog contains no valid models")
    return providers, dropped_count


def _read_document(raw: bytes) -> tuple[dict[str, dict[str, dict[str, Any]]], int]:
    return _validate_catalog(json.loads(raw.decode("utf-8")))


def _download() -> bytes:
    request = Request(URL, headers={"User-Agent": "purser"})
    with urlopen(request, timeout=TIMEOUT_SECONDS) as response:
        payload = response.read(MAX_RESPONSE_BYTES + 1)
    if len(payload) > MAX_RESPONSE_BYTES:
        raise ValueError("models.dev response exceeds size limit")
    return payload


def _atomic_write(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", prefix=path.name + ".", suffix=".tmp", dir=path.parent, delete=False
        ) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
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


def _cache_record(value: Any) -> tuple[dict[str, dict[str, dict[str, Any]]], str, int]:
    if not isinstance(value, dict) or not isinstance(value.get("fetched_at"), str):
        raise ValueError("invalid cache envelope")
    providers, dropped_count = _validate_catalog(value.get("providers"))
    recorded_dropped = value.get("dropped_count", 0)
    if isinstance(recorded_dropped, bool) or not isinstance(recorded_dropped, int) or recorded_dropped < 0:
        raise ValueError("invalid dropped count")
    fetched_at = value["fetched_at"]
    try:
        timestamp = datetime.fromisoformat(fetched_at.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError("invalid cache timestamp") from exc
    if timestamp.tzinfo is None:
        raise ValueError("cache timestamp must include a timezone")
    return providers, fetched_at, dropped_count + recorded_dropped


def _read_cache(path: Path) -> tuple[dict[str, dict[str, dict[str, Any]]], str, int]:
    with path.open("rb") as handle:
        raw = handle.read(MAX_RESPONSE_BYTES + 1)
    if len(raw) > MAX_RESPONSE_BYTES:
        raise ValueError("model catalog cache exceeds size limit")
    return _cache_record(json.loads(raw.decode("utf-8")))


def load_catalog(*, root: str | os.PathLike[str] | None = None) -> dict[str, Any]:
    """Return checked facts, preferring the network and preserving last good data."""
    cache_path = (Path(root) if root is not None else state_root.default_state_root()) / "model-catalog" / "catalog.json"
    try:
        providers, dropped_count = _read_document(_download())
        fetched_at = _utc_now()
        cached_providers = {provider_id: {"models": models} for provider_id, models in providers.items()}
        _atomic_write(cache_path, {
            "fetched_at": fetched_at,
            "dropped_count": dropped_count,
            "providers": cached_providers,
        })
        return {
            "source": "network",
            "fetched_at": fetched_at,
            "providers": providers,
            "dropped_count": dropped_count,
        }
    except (OSError, HTTPException, TimeoutError, ValueError, UnicodeError, json.JSONDecodeError, RecursionError):
        pass

    try:
        providers, fetched_at, dropped_count = _read_cache(cache_path)
    except (OSError, ValueError, UnicodeError, json.JSONDecodeError, RecursionError):
        providers, dropped_count = _validate_catalog(json.loads(_BOOTSTRAP_PATH.read_text(encoding="utf-8")))
        return {"source": "bootstrap", "fetched_at": None, "providers": providers, "dropped_count": dropped_count}
    return {"source": "cache", "fetched_at": fetched_at, "providers": providers, "dropped_count": dropped_count}
