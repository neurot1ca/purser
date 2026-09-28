import json
from pathlib import Path
import subprocess

import codex
import coder_config
import mechanical_learning
import telemetry_schema


def _inventory(root: Path, *models: str) -> Path:
    path = root / "opencode" / "candidates.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"refreshed_at": "now", "candidates": [
        {"id": model, "status": "measured" if index == 0 else "unmeasured", "added_at": "now"}
        for index, model in enumerate(models)
    ]}), encoding="utf-8")
    return path


def _accepted_bundle():
    return {
        "validation": "PASS",
        "micro_repair": None,
        "micro_repair_cleared_review": False,
        "reviewer1_result": codex.policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        "reviewer1_skipped_reason": None,
        "implementer_report": "Changed the parser to retain valid free model candidates.",
        "route": "mechanical",
    }


def test_shadow_uses_only_unranked_or_under_sampled_candidate(tmp_path):
    path = _inventory(tmp_path, "vendor/served-free", "vendor/ranked-free", "vendor/learning-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][1]["mechanical_samples"] = [True] * mechanical_learning.MIN_SAMPLES
    value["candidates"][2]["mechanical_samples"] = [True, False]
    path.write_text(json.dumps(value), encoding="utf-8")

    assert mechanical_learning.shadow_candidate("vendor/served-free", root=tmp_path) == "vendor/learning-free"


def test_shadow_obeys_opt_in_and_starts_for_accepted_mechanical_result(tmp_path, monkeypatch):
    _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    starts = []
    monkeypatch.setattr(mechanical_learning, "start_shadow", lambda **kwargs: starts.append(kwargs) or True)
    pending = {"task_id": "task", "project_key": "0123456789abcdef", "repo_root": str(tmp_path),
               "route": "mechanical", "model": "vendor/served-free", "agent": "opencode",
               "mechanical_opencode": False, "shadow_prompt": "Do the task."}
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    assert not starts

    pending["mechanical_opencode"] = True
    pending["shadow_prompt"] = "Do the task."
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    assert len(starts) == 1
    assert starts[0]["served_model"] == "vendor/served-free"


def test_shadow_env_asks_for_all_confined_tools_and_uses_scratch(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    source = tmp_path / "checkout"
    source.mkdir()
    (source / "original.txt").write_text("original", encoding="utf-8")
    captured = {}

    class Launched:
        pid = 123

    def popen(args, **kwargs):
        captured["path"] = Path(args[-1])
        captured["job"] = json.loads(Path(args[-1]).read_text(encoding="utf-8"))
        return Launched()

    monkeypatch.setattr(mechanical_learning.subprocess, "Popen", popen)
    assert mechanical_learning.start_shadow(project_key="0123456789abcdef", repository=str(source),
        prompt="Do the task.", accepted_report="same report", served_model="vendor/served-free", state=tmp_path)
    assert not mechanical_learning.start_shadow(project_key="0123456789abcdef", repository=str(source),
        prompt="Do the task.", accepted_report="same report", served_model="vendor/served-free", state=tmp_path)
    job = captured["job"]
    captured_run = {}
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    def run(args, **kwargs):
        captured_run.update(kwargs)
        (Path(kwargs["cwd"]) / "not-in-checkout.txt").write_text("shadow", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0,
            '{"type":"text","part":{"text":"same report"}}\n'
            '{"type":"step_finish","part":{"reason":"stop"}}\n', "")
    monkeypatch.setattr(mechanical_learning.subprocess, "run", run)
    assert mechanical_learning.run_shadow(captured["path"])
    assert not (source / "not-in-checkout.txt").exists()
    config = json.loads(captured_run["env"]["OPENCODE_CONFIG_CONTENT"])
    confined = {"edit", "write", "bash", "webfetch", "task", "external_directory"}
    assert set(config["permission"]) == confined
    assert set(config["permission"].values()) == {"ask"}
    assert config["agent"]["purser-shadow"]["mode"] == "primary"
    assert set(config["agent"]["purser-shadow"]["permission"]) == confined
    assert "deny" not in json.dumps(config)
    assert captured_run["cwd"] != str(source)
    assert not Path(job["scratch"]).exists()
    assert json.loads(path.read_text(encoding="utf-8"))["candidates"][1]["status"] == "unmeasured"


def test_shadow_scoring_failures_and_matching_report(tmp_path, monkeypatch):
    report = "Changed the parser to retain valid free model candidates."
    source = tmp_path / "checkout"
    source.mkdir()
    outcomes = [subprocess.TimeoutExpired("opencode", 1),
                subprocess.CompletedProcess([], 1, "HTTP 403", ""),
                subprocess.CompletedProcess([], 0, "", ""),
                subprocess.CompletedProcess([], 0,
                    '{"type":"text","part":{"text":"' + report + '"}}\n'
                    '{"type":"step_finish","part":{"reason":"stop"}}\n', "")]
    values = []
    monkeypatch.setattr(mechanical_learning, "record_sample", lambda model, success, **kwargs: values.append(success))
    monkeypatch.setattr(mechanical_learning, "_copy_scratch", lambda src, dest: dest.mkdir())
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    monkeypatch.setattr(mechanical_learning.subprocess, "run", lambda *args, **kwargs: _next(outcomes))
    for idx in range(4):
        parent = tmp_path / f"project-{idx}"
        parent.mkdir()
        (parent / "running.lock").write_text("1", encoding="utf-8")
        job = parent / "job.json"
        job.write_text(json.dumps({"model": "vendor/shadow-free", "repository": str(source),
            "prompt": "Do the task.", "accepted_report": report, "scratch": str(parent / "run"),
            "project_key": "0123456789abcdef", "state_root": str(tmp_path)}), encoding="utf-8")
        assert mechanical_learning.run_shadow(job) is (idx == 3)
    assert values == [False, False, False, True]


def _next(outcomes):
    value = outcomes.pop(0)
    if isinstance(value, BaseException):
        raise value
    return value


def test_ranking_waits_for_minimum_then_reorders(tmp_path):
    path = _inventory(tmp_path, "vendor/low-free", "vendor/high-free")
    for _ in range(mechanical_learning.MIN_SAMPLES - 1):
        mechanical_learning.record_sample("vendor/high-free", True, root=tmp_path)
        mechanical_learning.record_sample("vendor/low-free", False, root=tmp_path)
    value = json.loads(path.read_text(encoding="utf-8"))
    assert [item["id"] for item in value["candidates"]] == ["vendor/low-free", "vendor/high-free"]
    assert value["candidates"][1]["status"] == "unmeasured"
    mechanical_learning.record_sample("vendor/high-free", True, root=tmp_path)
    mechanical_learning.record_sample("vendor/low-free", False, root=tmp_path)
    value = json.loads(path.read_text(encoding="utf-8"))
    assert [item["id"] for item in value["candidates"]] == ["vendor/high-free", "vendor/low-free"]
    assert value["candidates"][0]["status"] == "measured"


def test_served_mechanical_telemetry_feeds_rank_samples(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    for _ in range(mechanical_learning.MIN_SAMPLES - 1):
        mechanical_learning.record_sample("vendor/served-free", True, root=tmp_path)
    monkeypatch.setattr(mechanical_learning, "start_shadow", lambda **kwargs: False)
    pending = {"task_id": "task", "project_key": "0123456789abcdef", "repo_root": str(tmp_path),
               "route": "mechanical", "model": "vendor/served-free", "agent": "opencode",
               "mechanical_opencode": True, "shadow_prompt": "Do the task."}
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    value = json.loads(path.read_text(encoding="utf-8"))
    candidate = next(item for item in value["candidates"] if item["id"] == "vendor/served-free")
    assert candidate["mechanical_samples"] == [True] * mechanical_learning.MIN_SAMPLES


def test_rank_change_telemetry_schema_is_valid():
    event = telemetry_schema.build_event("MECHANICAL_MODEL_RANK_CHANGED", project_key="0123456789abcdef",
        payload={"served_model": "vendor/model-free", "old_rank": 2, "new_rank": 1})
    assert telemetry_schema.valid_event(event)
