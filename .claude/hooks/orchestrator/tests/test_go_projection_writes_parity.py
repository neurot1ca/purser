import json
from pathlib import Path
import shutil
import sys


ROOT = Path(__file__).resolve().parents[4]
HOOKS = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = HOOKS / "tests" / "fixtures" / "go_policy" / "projection_writes.json"
sys.path.insert(0, str(HOOKS))
import telemetry
import telemetry_schema


STAMP = "2026-10-04T12:00:00Z"


def _oracle(case, aggregate):
    base = ROOT / ".tmp" / ("p58-" + case["name"])
    shutil.rmtree(base, ignore_errors=True)
    state = base / "state"
    state_telemetry = state / "telemetry"
    project_tasks = state / "projects" / "p58-project" / "telemetry" / "tasks"
    vault = base / "vault"
    project_tasks.mkdir(parents=True, exist_ok=True)
    (project_tasks / "p58-task.json").write_text(json.dumps(aggregate), encoding="utf-8")
    (state_telemetry / "tasks").mkdir(parents=True, exist_ok=True)
    health = state_telemetry / "telemetry_health.json"
    if case.get("health_directory"):
        health.mkdir(parents=True, exist_ok=True)
    else:
        health.write_text(json.dumps(case.get("initial_health", {})), encoding="utf-8")
    if case.get("run_note_directory"):
        run_dir = vault / "Runs" / "2026" / "10" / "p58-task.md"
        run_dir.mkdir(parents=True, exist_ok=True)
    if case.get("health_note_directory"):
        (vault / "System" / "Telemetry Health.md").mkdir(parents=True, exist_ok=True)

    original_now = telemetry_schema.utc_now
    telemetry_schema.utc_now = lambda: STAMP
    try:
        telemetry._project_task(
            "p58-task", state_root=state_telemetry, project_key="p58-project",
            projection_enabled=case.get("enabled", True), vault_root=vault, project_root=None,
        )
    finally:
        telemetry_schema.utc_now = original_now

    def read(path):
        try:
            return path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            return None

    try:
        health_json = json.loads(health.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        health_json = None
    run_note = None
    run_parent = vault / "Runs" / "2026" / "10"
    if run_parent.exists() and (run_parent / "p58-task.md").is_file():
        run_note = read(run_parent / "p58-task.md")
    return {
        "run_note": run_note,
        "health_json": health_json,
        "health_note": read(vault / "System" / "Telemetry Health.md"),
    }


def test_p58_python_projection_write_oracle():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    aggregate = fixture["aggregate"]
    actual = []
    for case in fixture["cases"]:
        actual.append({"name": case["name"], **_oracle(case, aggregate)})
    assert actual == fixture["expected"]
