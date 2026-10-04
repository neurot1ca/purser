import json
import os
import sys
from datetime import datetime, timedelta
from pathlib import Path


ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "quota.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex
import codex_quota


def _quota_home(case, home, now):
    for item in case["files"]:
        day = now.date() - timedelta(days=item["day"])
        folder = home / "sessions" / day.strftime("%Y/%m/%d")
        folder.mkdir(parents=True, exist_ok=True)
        path = folder / item["name"]
        if "prefix_event" in item:
            content = json.dumps(item["prefix_event"]) + "\n" + ("x" * item["padding_bytes"])
        else:
            content = "\n".join(json.dumps(event) for event in item["events"]) + "\n"
        path.write_text(content, encoding="utf-8")
        os.utime(path, (item["mtime"], item["mtime"]))


def test_p35_python_oracle_golden_fixture(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    now = datetime.fromisoformat(fixture["quota_now"])
    for case in fixture["quota_cases"]:
        assert "p35" in case["name"]
        home = tmp_path / case["name"]
        _quota_home(case, home, now)
        assert codex_quota.read_latest_quota(codex_home=home, now=now) == case["expected"], case["name"]

    for case in fixture["launch_cases"]:
        assert "p35" in case["name"]
        root = tmp_path / case["name"]
        indexes, jobs = root / "async_jobs", root / "codex_exec_mcp" / "jobs"
        indexes.mkdir(parents=True)
        jobs.mkdir(parents=True)
        for record in case["records"]:
            job_id = record["id"]
            (indexes / f"{job_id}.json").write_text(json.dumps({
                "jobId": job_id, "project_key": record["project_key"],
            }), encoding="utf-8")
            (jobs / f"{job_id}.json").write_text(json.dumps(record["job"]), encoding="utf-8")
        assert codex._last_launch_failure(root, "project") == case["expected"], case["name"]
