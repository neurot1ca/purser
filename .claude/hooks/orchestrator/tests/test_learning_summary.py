from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import learning_summary


def _source(root: Path, task_id: str, *, review: str = "passed", route: str = "normal"):
    jobs = root / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True, exist_ok=True)
    job_id = f"{len(list(jobs.glob('*.json'))):064x}"
    job = {"jobId": job_id, "taskId": task_id, "state": "SUCCEEDED",
           "acceptedAt": 10, "finishedAt": 20, "durationMs": 1000,
           "route": route, "jobModel": "model-a", "jobEffort": "high",
           "reviewerOutcome": review, "validation": "PASS", "applyConflictReason": None}
    path = jobs / f"{job_id}.json"
    path.write_text(json.dumps(job), encoding="utf-8")
    return path, job


def test_overflow_stays_within_record_cohort_sample_and_byte_caps(tmp_path):
    sources = [_source(tmp_path, f"task-{i}", route=f"route-{i}") for i in range(1200)]
    for _ in range(40):
        learning_summary.distill_jobs(tmp_path, sources, now=100)
    data = learning_summary.read_summary(tmp_path)
    assert len(data["records"]) <= 1024
    assert all(len(json.dumps(record, separators=(",", ":")).encode()) <= 2048
               for record in data["records"])
    assert len(data["cohorts"]) <= 256
    assert all(len(cell["samples"]) <= 64 for cell in data["cohorts"].values())
    assert "other" in data["cohorts"]
    assert (tmp_path / "telemetry" / learning_summary.SUMMARY_NAME).stat().st_size <= 4 * 1024 * 1024


def test_interrupted_checkpoint_retry_does_not_double_count(tmp_path, monkeypatch):
    source = _source(tmp_path, "task-one")
    original = learning_summary._replace_checkpoint
    monkeypatch.setattr(learning_summary, "_replace_checkpoint",
                        lambda *_args: (_ for _ in ()).throw(OSError("interrupted")))
    with pytest.raises(OSError):
        learning_summary.distill_jobs(tmp_path, [source], now=100)
    monkeypatch.setattr(learning_summary, "_replace_checkpoint", original)
    learning_summary.distill_jobs(tmp_path, [source], now=100)
    learning_summary.distill_jobs(tmp_path, [source], now=100)
    data = learning_summary.read_summary(tmp_path)
    assert data["cohorts"]["unknown|normal|model-a|high"]["counts"]["tasks"] == 1


def test_unavailable_review_is_distinct_from_passed(tmp_path):
    unavailable = _source(tmp_path, "task-a", review="unavailable")
    skipped = _source(tmp_path, "task-b", review="skipped")
    learning_summary.distill_jobs(tmp_path, [unavailable, skipped], now=100)
    cell = next(iter(learning_summary.read_summary(tmp_path)["cohorts"].values()))
    outcomes = cell["counts"]["review"]
    assert outcomes.get("passed", 0) == 0
    assert outcomes["unavailable"] == 1
    assert outcomes["skipped"] == 1
