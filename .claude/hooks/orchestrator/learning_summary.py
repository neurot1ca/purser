"""Bounded, at-most-once learning distilled before async evidence retirement."""

from __future__ import annotations

import json
import hashlib
import os
from pathlib import Path
import threading
import time
from typing import Any, Mapping

import telemetry

SUMMARY_NAME = "learning_summary.json"
LOCK_NAME = "learning_summary.lock"
MAX_SUMMARY_BYTES = 4 * 1024 * 1024
MAX_TASK_RECORDS = 1024
MAX_TASK_BYTES = 2048
MAX_COHORTS = 256
MAX_SAMPLES = 64
MAX_DISTILL_PER_RUN = 512
MAX_SOURCE_IDS = 16384
_OTHER = "other"
_LOCAL_LOCK = threading.RLock()


def _path(root: Path) -> Path:
    return root / "telemetry" / SUMMARY_NAME


def source_key(identity: str) -> str:
    if len(identity) == 64 and all(char in "0123456789abcdef" for char in identity):
        return identity
    return "opaque:" + hashlib.sha256(identity.encode("utf-8", errors="surrogatepass")).hexdigest()


def _empty() -> dict[str, Any]:
    return {"schema_version": 1, "sources": [], "records": [], "cohorts": {}}


def read_summary(state_root: str | os.PathLike[str]) -> dict[str, Any]:
    try:
        value = json.loads(_path(Path(state_root)).read_text(encoding="utf-8"))
        if (isinstance(value, dict) and value.get("schema_version") == 1
                and isinstance(value.get("sources"), list)
                and isinstance(value.get("records"), list)
                and isinstance(value.get("cohorts"), dict)):
            return value
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        pass
    return _empty()


def _replace_checkpoint(path: Path, encoded: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    try:
        with temp.open("wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        if os.name != "nt":
            descriptor = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
    finally:
        temp.unlink(missing_ok=True)


def _locked(path: Path):
    class Lock:
        def __enter__(self):
            _LOCAL_LOCK.acquire()
            path.parent.mkdir(parents=True, exist_ok=True)
            self.handle = (path.parent / LOCK_NAME).open("a+b")
            self.handle.seek(0, os.SEEK_END)
            if self.handle.tell() == 0:
                self.handle.write(b"0")
                self.handle.flush()
            self.handle.seek(0)
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(self.handle.fileno(), msvcrt.LK_LOCK, 1)
            else:
                import fcntl
                fcntl.flock(self.handle.fileno(), fcntl.LOCK_EX)
            return self

        def __exit__(self, *_args):
            try:
                if os.name == "nt":
                    import msvcrt
                    self.handle.seek(0)
                    msvcrt.locking(self.handle.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
                self.handle.close()
            finally:
                _LOCAL_LOCK.release()
    return Lock()


def _valid_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _task(root: Path, job: Mapping[str, Any]) -> dict[str, Any]:
    task_id = job.get("taskId")
    if not isinstance(task_id, str) or not task_id:
        return {}
    telemetry_root = root / "telemetry"
    index_path = root / "async_jobs" / f"{job.get('jobId')}.json"
    try:
        index = json.loads(index_path.read_text(encoding="utf-8"))
        project = index.get("project_key")
        if not isinstance(project, str):
            return {}
        events, _offset = telemetry._task_index_events(telemetry_root, task_id, project, 0)
        task = telemetry._build_task(telemetry_root, task_id, project, events, incremental=False)
        for source, target in (("reviewer1_valid_count", "reviewer1_valid_count"),
                               ("reviewer1_false_count", "reviewer1_false_count")):
            values = [event["payload"].get(source) for event in events
                      if event.get("event_type") == "CLAUDE_DECISION_RECORDED"
                      and _valid_number(event.get("payload", {}).get(source))]
            if values:
                task[target] = sum(values)
        return task
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError):
        return {}


def _record(job: Mapping[str, Any], task: Mapping[str, Any], stamp: float) -> dict[str, Any]:
    acceptance = task.get("claude_acceptance")
    acceptance = acceptance if isinstance(acceptance, Mapping) else {}
    review = job.get("reviewerOutcome")
    skipped = task.get("reviewer1_skipped_reason")
    result = task.get("reviewer1_result")
    if review not in {"passed", "failed", "skipped", "unavailable"}:
        if skipped == "REVIEWER1_UNAVAILABLE":
            review = "unavailable"
        elif skipped:
            review = "skipped"
        elif result is not None:
            review = "passed" if result in {"APPROVED", "PASS", "PASSED"} else "failed"
        else:
            review = None
    duration = task.get("duration_ms")
    if not _valid_number(duration):
        duration = job.get("durationMs")
    if not _valid_number(duration):
        duration = None
    eta = job.get("etaMinutes")
    eta_error = (duration - eta * 60000 if _valid_number(duration) and _valid_number(eta) else None)
    cohort = [task.get("project_key", job.get("project_key", "unknown")), job.get("route", task.get("route")),
              job.get("jobModel", task.get("model")), job.get("jobEffort", task.get("effort"))]
    cohort_key = "|".join((str(part) if part is not None else "unknown")[:64] for part in cohort)
    usage = task.get("usage") if isinstance(task.get("usage"), Mapping) else job.get("usage")
    usage = dict(usage) if isinstance(usage, Mapping) else None
    usage_complete = None
    if usage is not None:
        for field in ("claude_task_usage_complete", "claude_usage_known", "codex_usage_known",
                      "gemini_usage_known", "deepseek_usage_known"):
            if isinstance(usage.get(field), bool):
                usage_complete = usage[field]
                break
    quota = task.get("quota_samples", job.get("quota_samples"))
    if quota is None and isinstance(usage, Mapping):
        quota = [{"timestamp": usage.get(f"_claude_{window}_{edge}_captured_at"),
                  "resets_at": usage.get(f"_claude_{window}_{edge}_resets_at"),
                  "used": usage.get(f"claude_{window}_{edge}_pct")}
                 for window in ("5h", "7d") for edge in ("start", "end")
                 if usage.get(f"claude_{window}_{edge}_pct") is not None]
    if isinstance(quota, Mapping):
        quota = [quota]
    if isinstance(quota, list):
        quota = [{key: (sample[key][:64] if isinstance(sample[key], str) else sample[key])
                  for key in ("timestamp", "resets_at", "used", "limit")
                  if key in sample and (isinstance(sample[key], (str, int, float, bool))
                                        or sample[key] is None)}
                 for sample in quota[-MAX_SAMPLES:] if isinstance(sample, Mapping)
                 and sample.get("timestamp") is not None and sample.get("resets_at") is not None]
    else:
        quota = None
    record = {
        "id": job.get("jobId"), "task": job.get("taskId"), "at": stamp,
        "cohort": cohort_key, "duration_ms": duration, "eta_error_ms": eta_error,
        "acceptance": acceptance.get("outcome"),
        "send_backs": (acceptance.get("send_backs") if acceptance.get("outcome") in {"accepted", "rejected"}
                       or (_valid_number(acceptance.get("send_backs")) and acceptance["send_backs"] > 0) else None),
        "review": review, "reviewer1_valid": job.get("reviewer1Valid", task.get("reviewer1_valid_count")),
        "reviewer1_false": job.get("reviewer1False", task.get("reviewer1_false_count")), "validation": task.get("validation", job.get("validation")),
        "apply_conflict": job.get("applyConflictReason") or ("conflict" if job.get("applyState") == "conflict" else None),
        "failure": job.get("errorCode"),
        "usage": usage, "usage_complete": usage_complete, "quota": quota,
    }
    record = {key: value for key, value in record.items() if value is not None}
    while len(json.dumps(record, separators=(",", ":"), ensure_ascii=True).encode("utf-8")) > MAX_TASK_BYTES:
        if isinstance(record.get("usage"), dict) and record["usage"]:
            record["usage"].pop(next(iter(record["usage"])))
        elif isinstance(record.get("quota"), list) and record["quota"]:
            record["quota"].pop(0)
        else:
            for key in ("task", "cohort", "id"):
                if isinstance(record.get(key), str) and len(record[key]) > 32:
                    record[key] = record[key][:32]
                    break
            else:
                break
    return record


def _cell() -> dict[str, Any]:
    return {"counts": {"tasks": 0, "acceptance": {}, "send_backs": None, "review": {}, "validation": {},
                        "reviewer1_valid": None, "reviewer1_false": None, "apply_conflicts": {},
                        "failures": {}, "token_usage": {}, "token_usage_complete": {}},
            "samples": [], "quota_samples": []}


def _count(mapping: dict[str, int], value: Any) -> None:
    if isinstance(value, str) and value:
        key = value[:64]
        if key not in mapping and len(mapping) >= 31:
            key = "other"
        mapping[key] = mapping.get(key, 0) + 1


def _merge_record(data: dict[str, Any], record: dict[str, Any]) -> None:
    cohorts = data["cohorts"]
    key = record["cohort"]
    if key not in cohorts and len(cohorts) >= MAX_COHORTS - 1:
        key = _OTHER
    cell = cohorts.setdefault(key, _cell())
    counts = cell["counts"]
    counts["tasks"] += 1
    _count(counts["acceptance"], record.get("acceptance"))
    send_backs = record.get("send_backs")
    if _valid_number(send_backs):
        counts["send_backs"] = (counts["send_backs"] or 0) + send_backs
    _count(counts["review"], record.get("review"))
    _count(counts["validation"], record.get("validation"))
    _count(counts["apply_conflicts"], record.get("apply_conflict"))
    _count(counts["failures"], record.get("failure"))
    for field in ("reviewer1_valid", "reviewer1_false"):
        value = record.get(field)
        if _valid_number(value):
            counts[field] = (counts[field] or 0) + value
    usage = record.get("usage")
    if isinstance(usage, Mapping):
        for name, value in usage.items():
            if name.endswith("tokens") and _valid_number(value):
                if name not in counts["token_usage"] and len(counts["token_usage"]) >= 31:
                    name = "other"
                counts["token_usage"][name] = counts["token_usage"].get(name, 0) + value
    complete = record.get("usage_complete")
    if isinstance(complete, bool):
        name = "true" if complete else "false"
        counts["token_usage_complete"][name] += 1
    samples = cell["samples"]
    sample = {name: record[name] for name in ("at", "duration_ms", "eta_error_ms") if name in record}
    if sample.get("duration_ms") is not None or sample.get("eta_error_ms") is not None:
        samples.append(sample)
        del samples[:-MAX_SAMPLES]
    quota = record.get("quota")
    if isinstance(quota, list):
        cell["quota_samples"].extend(quota)
        del cell["quota_samples"][:-MAX_SAMPLES]


def _encode(data: dict[str, Any]) -> bytes:
    while True:
        encoded = json.dumps(data, separators=(",", ":"), ensure_ascii=True).encode("utf-8")
        if len(encoded) <= MAX_SUMMARY_BYTES:
            return encoded
        if data["records"]:
            data["records"].pop(0)
            continue
        if not data["cohorts"]:
            raise ValueError("learning checkpoint metadata exceeds its byte cap")
        largest = max(data["cohorts"], key=lambda key: len(json.dumps(data["cohorts"][key])))
        cell = data["cohorts"][largest]
        changed = False
        for mapping in cell["counts"].values():
            if not isinstance(mapping, dict) or len(mapping) < 2:
                continue
            key = next((key for key in mapping if key != "other"), None)
            if key is None:
                continue
            value = mapping.pop(key)
            if _valid_number(value):
                mapping["other"] = mapping.get("other", 0) + value
            changed = True
            break
        if not changed:
            for name in ("samples", "quota_samples"):
                values = cell[name]
                if len(values) > 1:
                    del values[:len(values) // 2]
                    changed = True
                    break
        if not changed:
            raise ValueError("learning checkpoint cells exceed their byte cap")


def distill_jobs(state_root: str | os.PathLike[str], sources: list[tuple[Path, dict[str, Any]]],
                 *, now: float | None = None, max_sources: int = MAX_DISTILL_PER_RUN,
                 existing_sources: set[str] | None = None) -> set[str]:
    root = Path(state_root)
    path = _path(root)
    current = time.time() if now is None else now
    with _locked(path):
        data = read_summary(root)
        present = existing_sources if existing_sources is not None else {
            source_key(item[0].stem) for item in sources if item[0].exists()}
        data["sources"] = [item for item in data["sources"] if item in present]
        identities = set(data["sources"])
        records = {record.get("id") for record in data["records"]}
        processed = 0
        for record_path, job in sources:
            identity = source_key(record_path.stem)
            if processed >= min(MAX_DISTILL_PER_RUN, max(0, max_sources)) or len(identities) >= MAX_SOURCE_IDS:
                break
            if identity in identities:
                continue
            stamp = job.get("finishedAt")
            final_terminal = (job.get("state") in {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "EXPIRED"}
                              and _valid_number(stamp)
                              and identity == record_path.stem)
            record = _record(job, _task(root, job), stamp) if final_terminal else None
            # Source identity and summary share one atomic checkpoint; never merge a retry twice.
            if record is not None and identity not in records:
                _merge_record(data, record)
                data["records"].append(record)
                data["records"] = data["records"][-MAX_TASK_RECORDS:]
                records.add(identity)
            identities.add(identity)
            data["sources"].append(identity)
            processed += 1
        data["sources"] = sorted(identities)
        encoded = _encode(data)
        _replace_checkpoint(path, encoded)
    return identities


def is_distilled(state_root: str | os.PathLike[str], identity: str) -> bool:
    return identity in set(read_summary(state_root).get("sources", ()))


__all__ = ["LOCK_NAME", "MAX_COHORTS", "MAX_DISTILL_PER_RUN", "MAX_SAMPLES", "MAX_SUMMARY_BYTES",
           "MAX_TASK_BYTES", "MAX_TASK_RECORDS", "SUMMARY_NAME", "distill_jobs", "is_distilled",
           "read_summary", "source_key"]
