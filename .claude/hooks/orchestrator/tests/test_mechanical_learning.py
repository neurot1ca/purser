import json
import os
from pathlib import Path
import shutil
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor

import codex
import coder_config
import mechanical_learning
import state_root
import telemetry
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


def test_shadow_starts_for_accepted_mechanical_result_even_when_codex_served(tmp_path, monkeypatch):
    _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    starts = []
    monkeypatch.setattr(mechanical_learning, "start_shadow", lambda **kwargs: starts.append(kwargs) or True)
    pending = {"task_id": "task", "project_key": "0123456789abcdef", "repo_root": str(tmp_path),
               "route": "normal", "model": "gpt-6-sol", "agent": "codex",
               "mechanical_opencode": False, "shadow_prompt": "Do the task."}
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    assert not starts

    pending["mechanical_opencode"] = True
    pending["route"] = "mechanical"
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    assert len(starts) == 1
    assert starts[0]["served_model"] == ""
    assert starts[0]["prompt"] == "Do the task."

    pending.update(agent="opencode", model="vendor/served-free")
    codex._post_observations({"session_id": "session"}, tmp_path, pending, _accepted_bundle())
    assert len(starts) == 2
    assert starts[1]["served_model"] == "vendor/served-free"


def test_skipped_reviewer_uses_worker_validation_for_shadow_acceptance(tmp_path, monkeypatch):
    _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    starts = []
    monkeypatch.setattr(mechanical_learning, "start_shadow", lambda **kwargs: starts.append(kwargs) or True)
    pending = {"task_id": "task", "project_key": "0123456789abcdef", "repo_root": str(tmp_path),
               "route": "mechanical", "model": "vendor/served-free", "agent": "opencode",
               "mechanical_opencode": True, "shadow_prompt": "Do the task."}

    skipped_bundle = _accepted_bundle()
    skipped_bundle.update(validation="UNKNOWN", reviewer1_result=None, reviewer1_skipped_reason="NO_DELTA",
                          worker_validation={"status": "PASS"})
    codex._post_observations({"session_id": "session"}, tmp_path, pending, skipped_bundle)
    assert len(starts) == 1

    for status in ("FAIL", "UNKNOWN"):
        rejected_bundle = dict(skipped_bundle, worker_validation={"status": status})
        codex._post_observations({"session_id": "session"}, tmp_path, pending, rejected_bundle)
    assert len(starts) == 1


def test_shadow_env_asks_for_all_confined_tools_and_uses_scratch(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    source = tmp_path / "checkout"
    source.mkdir()
    (source / "original.txt").write_text("original", encoding="utf-8")
    captured = {}

    class Launched:
        pid = os.getpid()

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


def test_shadow_reclaims_dead_pid_lock_and_removes_stale_artifacts(tmp_path, monkeypatch):
    _inventory(tmp_path, "vendor/served-free", "vendor/shadow-free")
    source = tmp_path / "checkout"
    source.mkdir()
    shadows = tmp_path / "projects" / "0123456789abcdef" / "opencode_shadows"
    shadows.mkdir(parents=True)
    lock = shadows / "running.lock"
    lock.write_text("999999999", encoding="utf-8")
    stale_job = shadows / "stale.json"
    stale_job.write_text("{}", encoding="utf-8")
    stale_scratch = shadows / "run-stale"
    stale_scratch.mkdir()
    (stale_scratch / "partial.txt").write_text("partial", encoding="utf-8")
    old_time = time.time() - mechanical_learning.SHADOW_TIMEOUT_SECONDS - 601
    os.utime(lock, (old_time, old_time))
    class Launched:
        pid = os.getpid()

    monkeypatch.setattr(mechanical_learning.subprocess, "Popen", lambda *args, **kwargs: Launched())

    assert mechanical_learning.start_shadow(project_key="0123456789abcdef", repository=str(source),
        prompt="Do the task.", accepted_report="same report", served_model="vendor/served-free", state=tmp_path)
    assert not stale_job.exists()
    assert not stale_scratch.exists()
    assert lock.exists()


def test_windows_shadow_lock_staleness_uses_age_without_killing_pid(tmp_path, monkeypatch):
    lock = tmp_path / "running.lock"
    lock.write_text(str(os.getpid()), encoding="utf-8")
    def unexpected_kill(*args):
        raise AssertionError("os.kill must not run on Windows")

    monkeypatch.setattr(mechanical_learning.os, "name", "nt")
    monkeypatch.setattr(mechanical_learning.os, "kill", unexpected_kill)

    assert not mechanical_learning._shadow_lock_is_stale(lock)
    old_time = time.time() - mechanical_learning.SHADOW_TIMEOUT_SECONDS - 601
    os.utime(lock, (old_time, old_time))
    assert mechanical_learning._shadow_lock_is_stale(lock)


def test_shadow_scoring_failures_and_matching_report(tmp_path, monkeypatch):
    report = "Changed the parser to retain valid free model candidates."
    source = tmp_path / "checkout"
    source.mkdir()
    outcomes = [subprocess.TimeoutExpired("opencode", 1),
                subprocess.CompletedProcess([], 1, "HTTP 403", ""),
                subprocess.CompletedProcess([], 0, "", ""),
                subprocess.CompletedProcess([], 0,
                    '{"type":"text","part":{"text":"' + report + '"}}\n'
                    '{"type":"step_finish","part":{"reason":"stop","tokens":{"input":10,"output":4,"total":14}}}\n', "")]
    values = []
    events = []
    monkeypatch.setattr(mechanical_learning, "record_sample", lambda model, success, **kwargs: values.append(success))
    monkeypatch.setattr(telemetry, "emit", lambda event_type, **payload: events.append((event_type, payload)))
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
    shadow_events = [payload for event_type, payload in events if event_type == "SHADOW_FINISHED"]
    assert shadow_events[0]["status"] == "failed"
    assert "agreement" not in shadow_events[0]
    assert "input_tokens" not in shadow_events[0]
    assert shadow_events[2]["status"] == "failed"
    assert "agreement" not in shadow_events[2]
    assert shadow_events[3]["status"] == "completed"
    assert shadow_events[3]["agreement"] is True
    assert shadow_events[3]["input_tokens"] == 10
    assert shadow_events[3]["output_tokens"] == 4
    assert shadow_events[3]["total_tokens"] == 14


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


def test_rank_one_change_writes_mechanical_leader(tmp_path):
    path = _inventory(tmp_path, "vendor/old-free", "vendor/new-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][0]["mechanical_samples"] = [False] * mechanical_learning.MIN_SAMPLES
    path.write_text(json.dumps(value), encoding="utf-8")

    for _ in range(mechanical_learning.MIN_SAMPLES):
        mechanical_learning.record_sample("vendor/new-free", True, root=tmp_path)

    leader_path = tmp_path / "opencode" / "leader.json"
    assert json.loads(leader_path.read_text(encoding="utf-8")) == {
        "model": "vendor/new-free", "successes": mechanical_learning.MIN_SAMPLES,
        "sample_count": mechanical_learning.MIN_SAMPLES, "announced": False,
    }


def test_lower_rank_move_does_not_write_mechanical_leader(tmp_path):
    path = _inventory(tmp_path, "vendor/leader-free", "vendor/lower-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][0]["mechanical_samples"] = [True] * mechanical_learning.MIN_SAMPLES
    path.write_text(json.dumps(value), encoding="utf-8")

    for _ in range(mechanical_learning.MIN_SAMPLES):
        mechanical_learning.record_sample("vendor/lower-free", False, root=tmp_path)

    assert not (tmp_path / "opencode" / "leader.json").exists()


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


def _benchmark_fake_runner(chore_prompts, *, fail=None, timeout=None, paths=None):
    def runner(command, **kwargs):
        scratch = Path(kwargs["cwd"])
        if paths is not None:
            paths.append(scratch)
        prompt = command[-1]
        chore = next(item for item in chore_prompts if item["prompt"] == prompt)
        if chore["id"] == timeout:
            raise subprocess.TimeoutExpired(command, kwargs["timeout"])
        for relative, content in chore["expected_files"].items():
            (scratch / relative).write_text(content, encoding="utf-8")
        if chore["id"] == fail:
            (scratch / "unexpected.txt").write_text("outside scope", encoding="utf-8")
        return subprocess.CompletedProcess(command, 0, "", "")
    return runner


def _use_benchmark_scratch(tmp_path, monkeypatch):
    class Scratch:
        def __enter__(self):
            self.path = tmp_path
            self.path.mkdir(exist_ok=True)
            return str(self.path)

        def __exit__(self, *_):
            shutil.rmtree(self.path, ignore_errors=True)

    monkeypatch.setattr(mechanical_learning.tempfile, "TemporaryDirectory", lambda **_: Scratch())


def test_mechanical_benchmark_all_chores_pass(tmp_path, monkeypatch):
    chores = mechanical_learning.MECHANICAL_CHORES
    _use_benchmark_scratch(tmp_path, monkeypatch)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    result = mechanical_learning.run_mechanical_benchmark(
        "vendor/test-free", runner=_benchmark_fake_runner(chores))

    assert result["suite_version"] == mechanical_learning.MECHANICAL_SUITE_VERSION
    assert result["model"] == "vendor/test-free"
    assert result["passed_count"] == result["total"] == 6
    assert all(item["passed"] for item in result["chores"])


def test_mechanical_benchmark_allows_edits_without_changing_reviewer_policy(tmp_path, monkeypatch):
    import reviewer

    _use_benchmark_scratch(tmp_path, monkeypatch)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    chores = mechanical_learning.MECHANICAL_CHORES

    def runner(command, **kwargs):
        config = json.loads(kwargs["env"]["OPENCODE_CONFIG_CONTENT"])
        assert config["permission"]["edit"] == config["permission"]["write"] == "allow"
        assert all(config["permission"][key] == "ask"
                   for key in ("bash", "webfetch", "task", "external_directory"))
        assert config["agent"]["purser-shadow"]["permission"]["edit"] == "allow"
        assert config["agent"]["purser-shadow"]["permission"]["write"] == "allow"
        return _benchmark_fake_runner(chores)(command, **kwargs)

    result = mechanical_learning.run_mechanical_benchmark("vendor/test-free", runner=runner)

    assert result["passed_count"] == len(chores)
    assert reviewer._OPENCODE_CONFINED_PERMISSIONS["edit"] == "ask"
    assert reviewer._OPENCODE_CONFINED_PERMISSIONS["write"] == "ask"


def test_mechanical_benchmark_rejects_out_of_scope_edit(tmp_path, monkeypatch):
    chores = mechanical_learning.MECHANICAL_CHORES
    _use_benchmark_scratch(tmp_path, monkeypatch)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    result = mechanical_learning.run_mechanical_benchmark(
        "vendor/test-free", runner=_benchmark_fake_runner(chores, fail=chores[0]["id"]))

    assert result["passed_count"] == 5
    assert result["chores"][0] == {"id": chores[0]["id"], "passed": False, "reason": "out_of_scope_edit"}


def test_mechanical_benchmark_timeout_fails_and_removes_scratch(tmp_path, monkeypatch):
    chores = mechanical_learning.MECHANICAL_CHORES
    paths = []
    _use_benchmark_scratch(tmp_path, monkeypatch)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    result = mechanical_learning.run_mechanical_benchmark(
        "vendor/test-free", runner=_benchmark_fake_runner(chores, timeout=chores[0]["id"], paths=paths))

    assert result["chores"][0]["reason"] == "timeout"
    assert result["passed_count"] == 5
    assert all(not path.exists() for path in paths)


def test_mechanical_benchmark_runner_error_fails_and_removes_scratch(tmp_path, monkeypatch):
    chores = mechanical_learning.MECHANICAL_CHORES
    paths = []
    _use_benchmark_scratch(tmp_path, monkeypatch)

    def runner(command, **kwargs):
        paths.append(Path(kwargs["cwd"]))
        if command[-1] == chores[0]["prompt"]:
            raise RuntimeError("runner failed")
        return _benchmark_fake_runner(chores)(command, **kwargs)

    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    result = mechanical_learning.run_mechanical_benchmark("vendor/test-free", runner=runner)

    assert result["chores"][0]["reason"] == "runner_error"
    assert result["passed_count"] == 5
    assert all(not path.exists() for path in paths)


def test_benchmark_unscored_candidates_skips_saved_and_records_missing(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/scored-free", "vendor/missing-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][1]["mechanical_samples"] = [True, False]
    value["candidates"][0]["benchmarks"] = {
        mechanical_learning.MECHANICAL_SUITE_VERSION: {
            "passed_count": 4, "total": 6, "recorded_at": "earlier"}}
    path.write_text(json.dumps(value), encoding="utf-8")
    calls = []

    def benchmark(model, **_kwargs):
        calls.append(model)
        return {"suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
                "model": model, "passed_count": 5, "total": 6, "chores": []}

    monkeypatch.setattr(mechanical_learning, "run_mechanical_benchmark", benchmark)
    results = mechanical_learning.benchmark_unscored_candidates(root=tmp_path)

    assert calls == ["vendor/missing-free"]
    assert len(results) == 1
    assert mechanical_learning.benchmark_unscored_candidates(root=tmp_path) == []
    assert calls == ["vendor/missing-free"]
    value = json.loads(path.read_text(encoding="utf-8"))
    saved = next(item for item in value["candidates"] if item["id"] == "vendor/missing-free")
    assert saved["mechanical_samples"] == [True, False]
    record = saved["benchmarks"][mechanical_learning.MECHANICAL_SUITE_VERSION]
    assert record["passed_count"] == 5 and record["total"] == 6
    assert isinstance(record["recorded_at"], str) and record["recorded_at"]


def test_benchmark_infrastructure_failure_is_unscored_and_retried(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/unavailable-free")
    _use_benchmark_scratch(tmp_path / "scratch", monkeypatch)
    chores = mechanical_learning.MECHANICAL_CHORES
    failed_attempts = set()

    def runner(command, **kwargs):
        prompt = command[-1]
        chore = next(item for item in chores if item["prompt"] == prompt)
        if chore["id"] == chores[0]["id"] and chore["id"] not in failed_attempts:
            failed_attempts.add(chore["id"])
            return subprocess.CompletedProcess(command, 1, "", "Upstream request failed: Endpoint is unavailable.")
        if chore["id"] == chores[1]["id"] and chore["id"] not in failed_attempts:
            failed_attempts.add(chore["id"])
            return subprocess.CompletedProcess(command, 1, "", "")
        for relative, content in chore["expected_files"].items():
            (Path(kwargs["cwd"]) / relative).write_text(content, encoding="utf-8")
        return subprocess.CompletedProcess(command, 0, "", "")

    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    result = mechanical_learning.run_mechanical_benchmark("vendor/unavailable-free", runner=runner)
    assert result["chores"][0]["reason"] == "infrastructure"
    assert result["chores"][1]["reason"] == "infrastructure"
    assert not mechanical_learning.record_benchmark(result, root=tmp_path)
    value = json.loads(path.read_text(encoding="utf-8"))
    candidate = value["candidates"][0]
    assert "benchmarks" not in candidate
    assert mechanical_learning.benchmark_unscored_candidates(root=tmp_path, runner=runner)[0]["passed_count"] == 6
    assert mechanical_learning.benchmark_unscored_candidates(root=tmp_path, runner=runner) == []


def test_benchmark_normal_model_failure_is_recorded(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/model-failure-free")
    chores = mechanical_learning.MECHANICAL_CHORES
    _use_benchmark_scratch(tmp_path / "scratch", monkeypatch)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")

    result = mechanical_learning.benchmark_unscored_candidates(
        root=tmp_path, runner=_benchmark_fake_runner(chores, fail=chores[0]["id"]))[0]
    assert result["passed_count"] == 5
    assert result["chores"][0]["reason"] == "out_of_scope_edit"
    candidate = json.loads(path.read_text(encoding="utf-8"))["candidates"][0]
    assert candidate["benchmarks"][mechanical_learning.MECHANICAL_SUITE_VERSION]["passed_count"] == 5


def test_benchmark_worker_drains_batches_and_records_run(tmp_path, monkeypatch):
    path = _inventory(tmp_path, *(f"vendor/model-{index}-free" for index in range(5)))
    calls = []

    def benchmark(model, **_kwargs):
        calls.append(model)
        result = {"suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
                  "model": model, "passed_count": 5, "total": 6, "chores": []}
        mechanical_learning.record_benchmark(result, root=tmp_path)
        return result

    monkeypatch.setattr(mechanical_learning, "run_mechanical_benchmark", benchmark)
    mechanical_learning.run_benchmarks(tmp_path)
    saved = json.loads(path.read_text(encoding="utf-8"))["candidates"]
    assert len(calls) == 5
    assert all(mechanical_learning._has_current_benchmark(item) for item in saved)
    run_files = list((tmp_path / "opencode").glob("benchmark-*.jsonl"))
    events = [json.loads(line) for line in run_files[0].read_text(encoding="utf-8").splitlines()]
    assert events[0]["event"] == "start"
    assert sum(event["event"] == "candidate" for event in events) == 5
    assert events[-1]["event"] == "completed"


def test_benchmark_lock_uses_heartbeat_and_live_worker_blocks_start(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/model-free")
    lock = tmp_path / "opencode" / "benchmark.running.lock"
    lock.write_text(json.dumps({"pid": os.getpid(), "heartbeat": time.time()}), encoding="utf-8")
    assert not mechanical_learning._benchmark_lock_is_stale(lock)
    monkeypatch.setattr(mechanical_learning.subprocess, "Popen", lambda *args, **kwargs:
                       pytest.fail("live worker must block second start"))
    assert not mechanical_learning.start_benchmark(root=tmp_path)
    old_run = tmp_path / "opencode" / "benchmark-old-run.jsonl"
    old_run.write_text('{"event":"start","run_id":"old-run"}\n', encoding="utf-8")
    lock.write_text(json.dumps({"pid": 999999999, "run_id": "old-run", "heartbeat": time.time() -
                                mechanical_learning.SHADOW_TIMEOUT_SECONDS - 31}), encoding="utf-8")
    monkeypatch.setattr(mechanical_learning.subprocess, "Popen", lambda *args, **kwargs:
                       type("Launched", (), {"pid": os.getpid()})())
    assert mechanical_learning.start_benchmark(root=tmp_path)
    assert json.loads(old_run.read_text(encoding="utf-8").splitlines()[-1])["event"] == "interrupted"


def test_windows_benchmark_staleness_uses_heartbeat_without_pid_probe(tmp_path, monkeypatch):
    lock = tmp_path / "benchmark.running.lock"

    def unexpected_kill(*_args):
        raise AssertionError("Windows benchmark staleness must not probe PIDs")

    monkeypatch.setattr(mechanical_learning.os, "name", "nt")
    monkeypatch.setattr(mechanical_learning.os, "kill", unexpected_kill)
    lock.write_text(json.dumps({"pid": 999999999, "heartbeat": time.time() -
                                mechanical_learning.SHADOW_TIMEOUT_SECONDS -
                                mechanical_learning.BENCHMARK_LOCK_GRACE_SECONDS - 1}), encoding="utf-8")
    assert mechanical_learning._benchmark_lock_is_stale(lock)
    lock.write_text(json.dumps({"pid": 999999999, "heartbeat": time.time()}), encoding="utf-8")
    assert not mechanical_learning._benchmark_lock_is_stale(lock)


def test_benchmark_worker_exception_logs_failed_without_scoring(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/model-free")
    monkeypatch.setattr(mechanical_learning, "benchmark_unscored_candidates",
                        lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("worker exploded")))
    mechanical_learning.run_benchmarks(tmp_path)
    saved = json.loads(path.read_text(encoding="utf-8"))["candidates"][0]
    assert not mechanical_learning._has_current_benchmark(saved)
    run_file = next((tmp_path / "opencode").glob("benchmark-*.jsonl"))
    events = [json.loads(line) for line in run_file.read_text(encoding="utf-8").splitlines()]
    assert events[-1]["event"] == "failed"
    assert "worker exploded" in events[-1]["reason"]


def test_first_qualifying_benchmark_writes_mechanical_leader(tmp_path):
    path = _inventory(tmp_path, "vendor/benchmark-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][0]["status"] = "unmeasured"
    path.write_text(json.dumps(value), encoding="utf-8")

    assert mechanical_learning.record_benchmark({
        "model": "vendor/benchmark-free", "suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
        "passed_count": 5, "total": len(mechanical_learning.MECHANICAL_CHORES), "chores": [],
    }, root=tmp_path)

    assert json.loads((tmp_path / "opencode" / "leader.json").read_text(encoding="utf-8")) == {
        "model": "vendor/benchmark-free", "successes": 5,
        "sample_count": len(mechanical_learning.MECHANICAL_CHORES), "announced": False,
    }


def test_benchmark_qualifying_lower_seed_does_not_write_mechanical_leader(tmp_path):
    path = _inventory(tmp_path, "vendor/first-seed", "vendor/lower-seed")
    value = json.loads(path.read_text(encoding="utf-8"))
    for candidate in value["candidates"]:
        candidate["status"] = "unmeasured"
    value["candidates"][0]["benchmarks"] = {mechanical_learning.MECHANICAL_SUITE_VERSION: {
        "passed_count": 5, "total": len(mechanical_learning.MECHANICAL_CHORES),
    }}
    path.write_text(json.dumps(value), encoding="utf-8")

    assert mechanical_learning.record_benchmark({
        "model": "vendor/lower-seed", "suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
        "passed_count": 5, "total": len(mechanical_learning.MECHANICAL_CHORES), "chores": [],
    }, root=tmp_path)

    assert not (tmp_path / "opencode" / "leader.json").exists()


def test_benchmark_does_not_write_leader_when_measured_candidate_is_first(tmp_path):
    _inventory(tmp_path, "vendor/measured-free", "vendor/benchmark-free")

    assert mechanical_learning.record_benchmark({
        "model": "vendor/benchmark-free", "suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
        "passed_count": 5, "total": len(mechanical_learning.MECHANICAL_CHORES), "chores": [],
    }, root=tmp_path)

    assert not (tmp_path / "opencode" / "leader.json").exists()


def test_below_threshold_benchmark_does_not_write_mechanical_leader(tmp_path):
    path = _inventory(tmp_path, "vendor/benchmark-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][0]["status"] = "unmeasured"
    path.write_text(json.dumps(value), encoding="utf-8")

    assert mechanical_learning.record_benchmark({
        "model": "vendor/benchmark-free", "suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
        "passed_count": mechanical_learning.BENCHMARK_MIN_PASSED - 1,
        "total": len(mechanical_learning.MECHANICAL_CHORES), "chores": [],
    }, root=tmp_path)

    assert not (tmp_path / "opencode" / "leader.json").exists()


def test_benchmark_unscored_candidates_caps_candidates_per_run(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/one-free", "vendor/two-free", "vendor/three-free")
    calls = []

    def benchmark(model, **_kwargs):
        calls.append(model)
        return {"suite_version": mechanical_learning.MECHANICAL_SUITE_VERSION,
                "model": model, "passed_count": 5, "total": 6, "chores": []}

    monkeypatch.setattr(mechanical_learning, "run_mechanical_benchmark", benchmark)
    mechanical_learning.benchmark_unscored_candidates(root=tmp_path)
    assert calls == ["vendor/one-free", "vendor/two-free"]


def test_start_benchmark_launch_is_single_flight(tmp_path, monkeypatch):
    _inventory(tmp_path, "vendor/new-free")
    launches = []

    def popen(args, **kwargs):
        launches.append((args, kwargs))
        return type("Process", (), {"pid": 1234})()

    monkeypatch.setattr(mechanical_learning.subprocess, "Popen", popen)
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(lambda _index: mechanical_learning.start_benchmark(root=tmp_path), range(2)))
    assert sorted(results) == [False, True]
    assert len(launches) == 1
    args, kwargs = launches[0]
    assert args[1:3] == [str(Path(mechanical_learning.__file__).resolve()), "--benchmark"]
    assert args[3] == str(tmp_path)
    assert kwargs["stdin"] == mechanical_learning.subprocess.DEVNULL
    assert kwargs["stdout"].name.endswith(".log")
    assert kwargs["stderr"] == mechanical_learning.subprocess.STDOUT
    assert kwargs["creationflags"] == (0x08000000 | 0x00000200 if os.name == "nt" else 0)


def test_interrupted_benchmark_leaves_candidate_unscored_and_ineligible(tmp_path, monkeypatch):
    path = _inventory(tmp_path, "vendor/interrupted-free")
    value = json.loads(path.read_text(encoding="utf-8"))
    value["candidates"][0]["status"] = "unmeasured"
    path.write_text(json.dumps(value), encoding="utf-8")
    monkeypatch.setattr(state_root, "default_state_root", lambda: tmp_path)

    def interrupt(*_args, **_kwargs):
        raise KeyboardInterrupt

    monkeypatch.setattr(mechanical_learning, "run_mechanical_benchmark", interrupt)
    try:
        mechanical_learning.benchmark_unscored_candidates(root=tmp_path)
    except KeyboardInterrupt:
        pass
    else:
        raise AssertionError("benchmark interruption should propagate")

    candidate = json.loads(path.read_text(encoding="utf-8"))["candidates"][0]
    assert "benchmarks" not in candidate
    assert coder_config.opencode_candidates() == []
