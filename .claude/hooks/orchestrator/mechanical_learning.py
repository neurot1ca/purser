"""Fail-open sampling and ranking for free mechanical OpenCode models."""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from typing import Any
import uuid

import model_listing
import state_root


MIN_SAMPLES = 5
RECENT_SAMPLES = 20
SHADOW_TIMEOUT_SECONDS = 45
_TOKEN = re.compile(r"[a-z0-9]+")


def _shadow_lock_is_stale(lock: Path) -> bool:
    try:
        age = time.time() - lock.stat().st_mtime
    except OSError:
        return True
    too_old = age > SHADOW_TIMEOUT_SECONDS + 600
    if os.name == "nt":
        return too_old
    try:
        pid = int(lock.read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return True
    if pid <= 0:
        return True
    try:
        os.kill(pid, 0)
    except PermissionError:
        return too_old
    except ProcessLookupError:
        return True
    except OSError:
        return too_old
    return too_old


def _inventory(root: Path) -> tuple[Path, dict[str, Any]]:
    path = root / "opencode" / "candidates.json"
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict) or not isinstance(value.get("candidates"), list):
        raise ValueError("invalid OpenCode candidate inventory")
    return path, value


def sample_count(candidate: dict[str, Any]) -> int:
    samples = candidate.get("mechanical_samples", [])
    return len(samples) if isinstance(samples, list) else 0


def ranked_candidates(candidates: list[dict[str, Any]]) -> list[dict[str, Any]]:
    indexed = list(enumerate(candidates))
    def order(pair: tuple[int, dict[str, Any]]) -> tuple[int, float, int]:
        index, candidate = pair
        if candidate.get("status") == "failed":
            return (2, 0.0, index)
        samples = candidate.get("mechanical_samples", [])
        if not isinstance(samples, list) or len(samples) < MIN_SAMPLES:
            return (1, 0.0, index)
        rate = sum(sample is True for sample in samples) / len(samples)
        return (0, -rate, index)
    return [candidate for _, candidate in sorted(indexed, key=order)]


def record_sample(model: str, success: bool, *, root: Path | None = None) -> tuple[int, int] | None:
    """Record one category sample and reorder candidates only after MIN_SAMPLES."""
    state = root or state_root.default_state_root()
    path = state / "opencode" / "candidates.json"
    with model_listing._candidate_lock(path) as acquired:
        if not acquired:
            return None
        path, value = _inventory(state)
        before = [item.get("id") for item in value["candidates"] if isinstance(item, dict)]
        old_leader = next((item.get("id") for item in value["candidates"]
                           if isinstance(item, dict) and item.get("status") == "measured"), None)
        found = None
        for candidate in value["candidates"]:
            if isinstance(candidate, dict) and candidate.get("id") == model:
                found = candidate
                break
        if found is None:
            return None
        samples = found.get("mechanical_samples", [])
        if not isinstance(samples, list) or any(type(item) is not bool for item in samples):
            samples = []
        samples = (samples + [bool(success)])[-RECENT_SAMPLES:]
        found["mechanical_samples"] = samples
        if len(samples) >= MIN_SAMPLES and found.get("status") != "failed":
            found["status"] = "measured"
        value["candidates"] = ranked_candidates(value["candidates"])
        model_listing._atomic_write(path, value)
        after = [item.get("id") for item in value["candidates"] if isinstance(item, dict)]
        new_leader = next((item for item in value["candidates"]
                           if isinstance(item, dict) and item.get("status") == "measured"), None)
        if new_leader is not None and new_leader.get("id") != old_leader:
            try:
                samples = new_leader.get("mechanical_samples", [])
                if isinstance(samples, list) and all(type(sample) is bool for sample in samples):
                    leader_path = state / "opencode" / "leader.json"
                    model_listing._atomic_write(leader_path, {
                        "model": new_leader["id"],
                        "successes": sum(sample is True for sample in samples),
                        "sample_count": len(samples),
                        "announced": False,
                    })
            except BaseException:
                pass
        return (before.index(model) + 1, after.index(model) + 1) if model in before and model in after else None


def shadow_candidate(served_model: str, *, root: Path | None = None) -> str | None:
    try:
        _, value = _inventory(root or state_root.default_state_root())
        for candidate in value["candidates"]:
            if (isinstance(candidate, dict) and candidate.get("id") != served_model
                    and candidate.get("status") != "failed"
                    and sample_count(candidate) < MIN_SAMPLES):
                return candidate.get("id") if isinstance(candidate.get("id"), str) else None
    except (OSError, ValueError, TypeError):
        pass
    return None


def start_shadow(*, project_key: str, repository: str, prompt: str, accepted_report: str,
                 served_model: str, state: Path | None = None) -> bool:
    """Launch at most one detached, scratch-copy run per project."""
    root = state or state_root.default_state_root()
    model = shadow_candidate(served_model, root=root)
    if not model:
        return False
    shadows = root / "projects" / project_key / "opencode_shadows"
    shadows.mkdir(parents=True, exist_ok=True)
    lock = shadows / "running.lock"
    try:
        descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        if not _shadow_lock_is_stale(lock):
            return False
        for stale_job in shadows.glob("*.json"):
            stale_job.unlink(missing_ok=True)
        for stale_scratch in shadows.glob("run-*"):
            if stale_scratch.is_dir():
                shutil.rmtree(stale_scratch, ignore_errors=True)
        lock.unlink(missing_ok=True)
        try:
            descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            return False
    job = shadows / f"{uuid.uuid4().hex}.json"
    scratch = shadows / f"run-{uuid.uuid4().hex}"
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(str(os.getpid()))
        job.write_text(json.dumps({"model": model, "repository": repository, "prompt": prompt,
                                   "accepted_report": accepted_report, "scratch": str(scratch),
                                   "project_key": project_key, "state_root": str(root)},
                                   ensure_ascii=False), encoding="utf-8")
        flags = 0x00000008 | 0x00000200 if os.name == "nt" else 0
        launched = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--shadow", str(job)],
                                     cwd=str(Path(__file__).resolve().parent), stdin=subprocess.DEVNULL,
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                     creationflags=flags, start_new_session=os.name != "nt")
        lock.write_text(str(launched.pid), encoding="utf-8")
        return True
    except Exception:
        job.unlink(missing_ok=True)
        shutil.rmtree(scratch, ignore_errors=True)
        lock.unlink(missing_ok=True)
        return False


def reports_agree(candidate: str, accepted: str) -> bool:
    # Jaccard overlap over normalized words and numbers makes paraphrase checks deterministic; numeric disagreement always fails.
    left, right = set(_TOKEN.findall(candidate.lower())), set(_TOKEN.findall(accepted.lower()))
    numbers_left = {word for word in left if word.isdigit()}
    numbers_right = {word for word in right if word.isdigit()}
    return bool(left and right and numbers_left == numbers_right
                and len(left & right) / len(left | right) >= 0.5)


def _final_report(stdout: str) -> str:
    texts: list[str] = []
    completed = False
    for line in stdout.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict):
            raise ValueError("invalid OpenCode event")
        if event.get("type") == "error":
            raise ValueError("OpenCode reported an error")
        part = event.get("part")
        if event.get("type") == "text" and isinstance(part, dict) and isinstance(part.get("text"), str):
            texts.append(part["text"])
        if event.get("type") == "step_finish" and isinstance(part, dict) and part.get("reason") == "stop":
            completed = True
    report = "".join(texts).strip()
    if not completed or not report:
        raise ValueError("OpenCode returned no completed report")
    return report


def _copy_scratch(source: Path, scratch: Path) -> None:
    def safe_entries(directory: str, names: list[str]) -> set[str]:
        ignored = {name for name in names if Path(directory, name).is_symlink()
                   or name in {".git", "node_modules", ".venv", "venv", "__pycache__"}}
        return ignored
    shutil.copytree(source, scratch, ignore=safe_entries)


def run_shadow(job_path: Path) -> bool:
    job = json.loads(job_path.read_text(encoding="utf-8"))
    scratch = Path(job["scratch"])
    lock = job_path.parent / "running.lock"
    success = False
    try:
        lock.write_text(str(os.getpid()), encoding="utf-8")
        source = Path(job["repository"]).resolve()
        _copy_scratch(source, scratch)
        import reviewer
        permissions = dict(reviewer._OPENCODE_CONFINED_PERMISSIONS)
        config = {"permission": permissions,
                  "agent": {"purser-shadow": {"mode": "primary", "permission": permissions}}}
        import coder_config
        executable = coder_config.opencode_binary()
        if not executable:
            raise FileNotFoundError("OpenCode executable is unavailable")
        command = [executable, "run", "--format", "json", "--model", job["model"],
                   "--agent", "purser-shadow", "--dir", str(scratch), job["prompt"]]
        environment = {**os.environ, "OPENCODE_CONFIG_CONTENT": json.dumps(config, separators=(",", ":"))}
        result = subprocess.run(command, cwd=scratch, capture_output=True, text=True,
                                encoding="utf-8", errors="replace", timeout=SHADOW_TIMEOUT_SECONDS,
                                check=False, env=environment)
        report = _final_report(result.stdout) if result.returncode == 0 else ""
        success = bool(report and reports_agree(report, job["accepted_report"]))
    except Exception:
        success = False
    try:
        root = Path(job["state_root"])
        positions = record_sample(job["model"], success, root=root)
        if positions and positions[0] != positions[1]:
            import telemetry
            telemetry.emit("MECHANICAL_MODEL_RANK_CHANGED", state_root=root,
                           project_key=job["project_key"], served_model=job["model"],
                           old_rank=positions[0], new_rank=positions[1])
    except Exception:
        pass
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
        job_path.unlink(missing_ok=True)
        lock.unlink(missing_ok=True)
    return success


if __name__ == "__main__" and len(sys.argv) == 3 and sys.argv[1] == "--shadow":
    run_shadow(Path(sys.argv[2]))
