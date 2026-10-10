"""Durable task attempt evidence shared by the Python development hooks."""

from __future__ import annotations

from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import time
import uuid


MISSING, OK, CORRUPT = "MISSING", "OK", "CORRUPT"

def _path(root: Path, project_key: str, task_id: str) -> Path:
    key = hashlib.sha256((project_key + "\0" + task_id).encode("utf-8")).hexdigest()
    return root / "task_retries" / (key + ".json")


def load(root: Path, project_key: str, task_id: str) -> tuple[dict, str]:
    if not project_key or not task_id:
        return {}, CORRUPT
    try:
        value = json.loads(_path(root, project_key, task_id).read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}, MISSING
    except (OSError, UnicodeDecodeError, ValueError):
        return {}, CORRUPT
    if (not isinstance(value, dict) or value.get("project_key") != project_key
            or value.get("task_id") != task_id):
        return {}, CORRUPT
    return value, OK
@contextmanager
def _lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    if os.name != "nt":
        os.chmod(path.parent, 0o700)
    with path.open("a+b") as stream:
        if os.name != "nt":
            os.chmod(path, 0o600)
        deadline = time.monotonic() + 10
        if os.name == "nt":
            import msvcrt
            stream.seek(0)
            if not stream.read(1):
                stream.write(b"0"); stream.flush()
            stream.seek(0)
            while True:
                try:
                    msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
                    break
                except OSError:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("task attempt lock timed out")
                    time.sleep(0.02)
            try:
                yield
            finally:
                stream.seek(0); msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
        else:
            import fcntl
            while True:
                try:
                    fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("task attempt lock timed out")
                    time.sleep(0.02)
            try:
                yield
            finally:
                fcntl.flock(stream.fileno(), fcntl.LOCK_UN)

def save(root: Path, update: dict) -> None:
    project, task = update.get("project_key"), update.get("task_id")
    if not isinstance(project, str) or not project or not isinstance(task, str) or not task:
        return
    path = _path(root, project, task)
    with _lock(path.with_suffix(path.suffix + ".lock")):
        current, status = load(root, project, task)
        if status == CORRUPT:
            raise ValueError("task attempt record is corrupt")
        if status == MISSING:
            current = {"project_key": project, "task_id": task}
        for key in ("latest_job_id", "predecessor_job_id", "terminal_disposition"):
            if update.get(key):
                current[key] = update[key]
        if current.get("send_back_used") is True or update.get("send_back_used") is True:
            current["send_back_used"] = True
        else:
            current.pop("send_back_used", None)
        failure = update.get("failure")
        if (not current.get("failure") and isinstance(failure, dict)
                and failure.get("job_id")
                and failure.get("category") in {"rejected_after_sendback", "failed_abandonment"}):
            current["failure"] = failure
        path.parent.mkdir(parents=True, exist_ok=True)
        if os.name != "nt":
            os.chmod(path.parent, 0o700)
        temporary = path.with_name(path.name + "." + uuid.uuid4().hex + ".tmp")
        try:
            with temporary.open("x", encoding="utf-8") as stream:
                if os.name != "nt":
                    os.chmod(temporary, 0o600)
                json.dump(current, stream, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                stream.flush(); os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)

def start(root: Path, job: dict) -> None:
    repo = job.get("repoRoot")
    task = job.get("taskId")
    if not isinstance(repo, str) or not repo or not isinstance(task, str) or not task:
        return
    import project_root
    save(root, {"project_key": project_root.project_key(Path(repo).resolve()), "task_id": task,
                "latest_job_id": job.get("jobId"), "predecessor_job_id": job.get("predecessorJobId"),
                "send_back_used": bool(job.get("predecessorJobId")), "terminal_disposition": "started"})

def terminal(root: Path, job: dict, disposition: str, *, reason: str = "", reviewer_result: str = "") -> None:
    repo, task, job_id = job.get("repoRoot"), job.get("taskId"), job.get("jobId")
    if not isinstance(repo, str) or not repo or not isinstance(task, str) or not task:
        return
    import project_root
    project = project_root.project_key(Path(repo).resolve())
    previous, status = load(root, project, task)
    if status == CORRUPT:
        raise ValueError("task attempt record is corrupt")
    if (disposition == "discarded" and previous.get("send_back_used") is True
            and job.get("decisionRecorded") is True and not reason):
        disposition = "rejected"
    if reviewer_result:
        disposition = reviewer_result
    update = {"project_key": project, "task_id": task, "terminal_disposition": disposition}
    failure = None
    if disposition == "rejected" and previous.get("send_back_used") and _luna_implementation(job):
        failure = {"job_id": job_id, "category": "rejected_after_sendback"}
    elif disposition in {"cancelled", "discarded", "failed"} and reason == "failed" and _luna_implementation(job):
        failure = {"job_id": job_id, "category": "failed_abandonment"}
        update["terminal_disposition"] = "failed"
    if failure:
        update["failure"] = failure
    save(root, update)

def _luna_implementation(job: dict) -> bool:
    return (job.get("jobPhase", job.get("phase")) == "implementation"
            and job.get("route") in {"subtle", "mechanical"}
            and bool(re.fullmatch(r"gpt-6(?:\.\d+)*-luna", str(job.get("model", "")).lower())))
