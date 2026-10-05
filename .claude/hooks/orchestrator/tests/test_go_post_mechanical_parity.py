import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "enforce" / "testdata" / "post_mechanical.json"
sys.path.insert(0, str(ORCH))
import mechanical_learning
import model_listing


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


def test_python_post_mechanical_oracle_fixture(tmp_path, monkeypatch):
    names = ("sample_recorded_ranking_updated_launch_spec", "sample_write_failure",
             "launch_failure", "feature_disabled_by_config")
    actual = []
    for name in names:
        case = tmp_path / name
        case.mkdir()
        actual.append({"name": name, "result": _run_case(name, case, monkeypatch)})
        monkeypatch.undo()
    if os.environ.get("POST_MECHANICAL_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps({"oracle": "mechanical_learning.record_sample+start_shadow",
                                       "cases": actual}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "mechanical_learning.record_sample+start_shadow"
    assert actual == fixture["cases"]
