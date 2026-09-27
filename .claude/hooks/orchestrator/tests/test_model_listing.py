"""CLI listing fixtures captured locally on 2026-09-27 (Codex JSON trimmed to parser fields)."""

from pathlib import Path
from datetime import datetime, timedelta, timezone
import json
import subprocess

import model_listing


FIXTURES = Path(__file__).with_name("fixtures")


def test_real_cli_formats_and_unknown_effort():
    codex = model_listing.parse_codex((FIXTURES / "codex-models.json").read_text(encoding="utf-8"))
    assert [entry["id"] for entry in codex] == ["gpt-6-astra", "gpt-6-sol"]
    assert codex[0]["efforts"][-1] == "ultra"
    agy = model_listing.parse_agy((FIXTURES / "agy-models.txt").read_text(encoding="utf-8"))
    flash = next(entry for entry in agy if entry["id"] == "gemini-3.8-flash")
    assert flash["variants"]["medium"] == "gemini-3.8-flash-medium"
    assert flash["efforts"] == ["high", "medium", "low"]
    assert next(entry for entry in agy if entry["id"] == "claude-sonnet-4-6")["efforts"] is None
    opencode = model_listing.parse_opencode("openai/gpt-6-astra\nmagnitude/qwen3.5-4b:gguf:q5\n")
    assert opencode == [{"id": "openai/gpt-6-astra", "efforts": None}]


def test_unavailable_failure_and_timeout(monkeypatch):
    monkeypatch.setattr(model_listing.coder_config, "opencode_binary", lambda: "opencode.exe")
    def failed(*_args, **_kwargs):
        return subprocess.CompletedProcess([], 1, "", "listing failed")
    monkeypatch.setattr(model_listing.subprocess, "run", failed)
    assert model_listing.list_models("opencode", {})["status"] == "unavailable"
    def timed_out(*_args, **_kwargs):
        raise subprocess.TimeoutExpired("opencode models", 30)
    monkeypatch.setattr(model_listing.subprocess, "run", timed_out)
    assert model_listing.list_models("opencode", {})["reason"] == "listing timed out"


def test_exact_binding_and_unknown(monkeypatch):
    monkeypatch.setattr(model_listing.coder_config, "opencode_binary", lambda: "opencode.exe")
    monkeypatch.setattr(model_listing.subprocess, "run", lambda *_a, **_k: subprocess.CompletedProcess([], 0, "openai/gpt-6-astra\nother/gpt-6-astra\n", ""))
    providers = {"openai": {"gpt-6-astra": {"family": "gpt-6"}}}
    models = model_listing.list_models("opencode", providers)["models"]
    assert [entry["family"] for entry in models] == ["gpt-6", None]


def test_refresh_candidates_filters_free_and_preserves_order_status(monkeypatch, tmp_path):
    providers = {"opencode": {
        "catalog-free": {"cost": {"input": 0, "output": 0}},
        "catalog-paid": {"cost": {"input": 1, "output": 1}},
    }}
    ids = ["another/model-free", "opencode/catalog-free", "opencode/catalog-paid", "vendor/model-free"]
    monkeypatch.setattr(model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": model_id} for model_id in ids]})
    path = tmp_path / "opencode" / "candidates.json"
    path.parent.mkdir()
    path.write_text(json.dumps({"refreshed_at": "old", "candidates": [
        {"id": "vendor/model-free", "status": "measured", "added_at": "first"},
        {"id": "opencode/catalog-free", "status": "failed", "added_at": "second"},
        {"id": "opencode/removed-free", "status": "failed", "added_at": "second"},
    ]}), encoding="utf-8")

    assert model_listing.refresh_candidates(providers, root=tmp_path)
    value = json.loads(path.read_text(encoding="utf-8"))
    assert [item["id"] for item in value["candidates"]] == [
        "vendor/model-free", "opencode/catalog-free", "another/model-free"]
    assert value["candidates"][0]["status"] == "measured"
    assert value["candidates"][1]["status"] == "failed"
    assert value["candidates"][2]["status"] == "unmeasured"
    assert value["refreshed_at"].endswith("Z")


def test_failed_listing_leaves_candidate_inventory_unchanged(monkeypatch, tmp_path):
    monkeypatch.setattr(model_listing, "list_models", lambda *_: {"status": "unavailable", "models": []})
    path = tmp_path / "opencode" / "candidates.json"
    path.parent.mkdir()
    path.write_bytes(b'{"keep":"exact bytes"}')
    assert not model_listing.refresh_candidates({}, root=tmp_path)
    assert path.read_bytes() == b'{"keep":"exact bytes"}'
    monkeypatch.setattr(model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "malformed id"}]})
    assert not model_listing.refresh_candidates({}, root=tmp_path)
    assert path.read_bytes() == b'{"keep":"exact bytes"}'


def test_candidate_inventory_stale_after_24_hours(tmp_path):
    path = tmp_path / "opencode" / "candidates.json"
    path.parent.mkdir()
    now = datetime.now(timezone.utc)
    path.write_text(json.dumps({"refreshed_at": (now - timedelta(hours=23)).isoformat(), "candidates": []}), encoding="utf-8")
    assert not model_listing.stale(root=tmp_path)
    path.write_text(json.dumps({"refreshed_at": (now - timedelta(hours=25)).isoformat(), "candidates": []}), encoding="utf-8")
    assert model_listing.stale(root=tmp_path)
