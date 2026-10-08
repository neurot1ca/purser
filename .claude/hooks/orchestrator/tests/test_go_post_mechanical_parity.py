import json
import os
from pathlib import Path
import shutil
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "enforce" / "testdata" / "post_mechanical.json"
sys.path.insert(0, str(ORCH))
import mechanical_learning
import model_listing
import coder_config
import telemetry


def _inventory():
    return {"candidates": [
        {"id": "served", "status": "available", "mechanical_samples": [True, False, True, True]},
        {"id": "runner-up", "status": "measured", "mechanical_samples": [True, True, True, False, False]},
        {"id": "shadow", "status": "available", "mechanical_samples": []},
    ]}


def _run_case(name, tmp_path, monkeypatch):
    state = tmp_path / "state"
    candidates = state / "opencode" / "candidates.json"
    candidates.parent.mkdir(parents=True)
    candidates.write_text(json.dumps(_inventory()), encoding="utf-8")
    launches = []
    job_paths = []
    if name == "sample_write_failure":
        def fail_write(path, value):
            if Path(path) == candidates:
                raise OSError("fixture write failure")
            return original_write(path, value)
        original_write = model_listing._atomic_write
        monkeypatch.setattr(model_listing, "_atomic_write", fail_write)
    elif name == "launch_failure":
        monkeypatch.setattr(mechanical_learning.subprocess, "Popen",
                            lambda *args, **kwargs: (_ for _ in ()).throw(OSError("fixture launch failure")))
    elif name == "sample_recorded_ranking_updated_launch_spec":
        def capture(argv, **kwargs):
            job_paths.append(Path(argv[3]))
            launches.append({"argv": ["__PYTHON__", "__SCRIPT__", "--shadow", "__JOB__"], "env": kwargs.get("env"),
                             "cwd": "__ORCH__"})
            return type("Child", (), {"pid": 9876})()
        monkeypatch.setattr(mechanical_learning.subprocess, "Popen", capture)

    enabled = name != "feature_disabled_by_config"
    try:
        if enabled:
            positions = mechanical_learning.record_sample("served", True, root=state)
            if positions and positions[0] != positions[1]:
                pass  # The caller's rank-change observation is intentionally outside persisted state.
            if name in {"sample_recorded_ranking_updated_launch_spec", "launch_failure"}:
                mechanical_learning.start_shadow(project_key="fixture-project", repository="__REPO__",
                    prompt="fixture prompt", accepted_report="accepted", served_model="served", state=state)
    except BaseException:
        pass  # Same best-effort boundary as codex.post_tool_use.

    inventory = json.loads(candidates.read_text(encoding="utf-8")) if candidates.is_file() else None
    leader_path = state / "opencode" / "leader.json"
    leader = json.loads(leader_path.read_text(encoding="utf-8")) if leader_path.exists() else None
    if launches:
        job_data = json.loads(job_paths[0].read_text(encoding="utf-8"))
        job_data["scratch"] = "__SCRATCH__"
        job_data["state_root"] = "__STATE__"
        job_data["repository"] = "__REPO__"
    else:
        job_data = None
    return {"inventory": inventory, "leader": leader, "launch": launches[0] if launches else None,
            "job": job_data}


def _run_shadow_case(name, tmp_path, monkeypatch):
    state = tmp_path / "state"
    candidates = state / "opencode" / "candidates.json"
    candidates.parent.mkdir(parents=True)
    candidates.write_text(json.dumps({"candidates": [
        {"id": "served", "status": "measured", "mechanical_samples": [True, True, True, False, False]},
        {"id": "shadow", "status": "available", "mechanical_samples": [True] * 4},
    ]}), encoding="utf-8")
    source = tmp_path / "repository"
    source.mkdir()
    (source / "original.txt").write_text("unchanged", encoding="utf-8")
    shadow_dir = state / "projects" / "fixture-project" / "opencode_shadows"
    shadow_dir.mkdir(parents=True)
    scratch = shadow_dir / "run-fixture"
    job = shadow_dir / "fixture.json"
    job.write_text(json.dumps({"model": "shadow", "repository": str(source),
        "prompt": name, "accepted_report": "Changed parser to retain valid free candidates.",
        "scratch": str(scratch), "project_key": "fixture-project", "state_root": str(state)}),
        encoding="utf-8")
    lock = shadow_dir / "running.lock"
    lock.write_text("1234", encoding="utf-8")

    fake = tmp_path / "fake_opencode.py"
    fake.write_text(
        "import json, sys, time\n"
        "scenario = sys.argv[-1]\n"
        "if scenario == 'timeout': time.sleep(2)\n"
        "if scenario == 'success':\n"
        " print(json.dumps({'type':'text','part':{'text':'Changed parser to retain valid free candidates.'}}))\n"
        " print(json.dumps({'type':'step_finish','part':{'reason':'stop','tokens':{'input':10,'output':4,'total':14}}}))\n"
        "elif scenario == 'failure':\n"
        " print(json.dumps({'type':'step_finish','part':{'reason':'error','tokens':{'input_tokens':3,'output_tokens':2,'total_tokens':5}}}))\n"
        " sys.exit(1)\n", encoding="utf-8")
    if os.name == "nt":
        executable = tmp_path / "fake_opencode.cmd"
        executable.write_text(f'@echo off\n"{sys.executable}" "{fake}" %*\n', encoding="utf-8")
    else:
        executable = tmp_path / "fake_opencode"
        executable.write_text(f"#!{sys.executable}\n" + fake.read_text(encoding="utf-8"), encoding="utf-8")
        executable.chmod(0o755)
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: str(executable))
    if name == "timeout":
        monkeypatch.setattr(mechanical_learning, "SHADOW_TIMEOUT_SECONDS", 0.2)
    events = []
    monkeypatch.setattr(telemetry, "emit", lambda event, **payload: events.append({"event": event, **payload}))
    result = mechanical_learning.run_shadow(job)
    inventory = json.loads(candidates.read_text(encoding="utf-8"))
    clean_events = []
    for event in events:
        event.pop("elapsed_ms", None)
        if "state_root" in event:
            event["state_root"] = "__STATE__"
        clean_events.append(event)
    return {"result": result, "inventory": inventory,
            "artifacts_removed": {"job": not job.exists(), "scratch": not scratch.exists(),
                                  "lock": not lock.exists()},
            "repository_unchanged": (source / "original.txt").read_text(encoding="utf-8") == "unchanged"
                                    and len(list(source.iterdir())) == 1,
            "telemetry": clean_events}


def test_python_post_mechanical_oracle_fixture(tmp_path, monkeypatch):
    names = ("sample_recorded_ranking_updated_launch_spec", "sample_write_failure",
             "launch_failure", "feature_disabled_by_config")
    actual = []
    for name in names:
        case = tmp_path / name
        case.mkdir()
        actual.append({"name": name, "result": _run_case(name, case, monkeypatch)})
        monkeypatch.undo()
    shadow_cases = []
    scratch_root = ROOT / ".tmp"
    scratch_root.mkdir(exist_ok=True)
    for name in ("success", "failure", "timeout"):
        repeated = []
        for index in range(2):
            case = scratch_root / f"post-mechanical-{uuid.uuid4().hex}"
            case.mkdir()
            try:
                repeated.append(_run_shadow_case(name, case, monkeypatch))
            finally:
                monkeypatch.undo()
                shutil.rmtree(case, ignore_errors=True)
        assert repeated[0] == repeated[1]
        shadow_cases.append({"name": f"run_shadow_{name}", "result": repeated[0]})
    if os.environ.get("POST_MECHANICAL_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        if FIXTURE.is_file():
            existing = json.loads(FIXTURE.read_text(encoding="utf-8"))
            launches_by_name = {case["name"]: case["result"]["launch"]
                                for case in existing["cases"]}
            for case in actual:
                if case["name"] in launches_by_name:
                    case["result"]["launch"] = launches_by_name[case["name"]]
        FIXTURE.write_text(json.dumps({"oracle": "mechanical_learning.record_sample+start_shadow",
                                       "cases": actual, "shadow_cases": shadow_cases},
                          ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "mechanical_learning.record_sample+start_shadow"
    assert [case["result"]["launch"] is None for case in actual] == [
        case["result"]["launch"] is None for case in fixture["cases"]]
    actual_without_launch = [{**case, "result": {key: value for key, value in case["result"].items()
                                                   if key != "launch"}} for case in actual]
    fixture_without_launch = [{**case, "result": {key: value for key, value in case["result"].items()
                                                    if key != "launch"}} for case in fixture["cases"]]
    assert actual_without_launch == fixture_without_launch
    assert shadow_cases == fixture["shadow_cases"]
