"""Fail-open sampling and ranking for free mechanical OpenCode models."""

from __future__ import annotations

import json
import os
from datetime import datetime, timezone
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Any
import uuid

import model_listing
import state_root


MIN_SAMPLES = 5
BENCHMARK_MIN_PASSED = 5
MAX_BENCHMARK_CANDIDATES = 2
RECENT_SAMPLES = 20
SHADOW_TIMEOUT_SECONDS = 45
MECHANICAL_SUITE_VERSION = "1"
MECHANICAL_CHORES = (
    {"id": "rename_symbol", "prompt": "Rename old_total to new_total in both files.",
     "initial_files": {"main.py": "from helpers import old_total\nprint(old_total([1, 2]))\n",
                       "helpers.py": "def old_total(values):\n    return sum(values)\n"},
     "expected_files": {"main.py": "from helpers import new_total\nprint(new_total([1, 2]))\n",
                        "helpers.py": "def new_total(values):\n    return sum(values)\n"}},
    {"id": "add_import", "prompt": "Add the standard-library math import used by rounded_down.",
     "initial_files": {"calc.py": "def rounded_down(value):\n    return math.floor(value)\n"},
     "expected_files": {"calc.py": "import math\n\ndef rounded_down(value):\n    return math.floor(value)\n"}},
    {"id": "fix_string_typo", "prompt": "Fix the typo in the user-facing message.",
     "initial_files": {"messages.py": "MESSAGE = \"Please recieve a receipt.\"\n"},
     "expected_files": {"messages.py": "MESSAGE = \"Please receive a receipt.\"\n"}},
    {"id": "update_config", "prompt": "Change the retry count from 2 to 3.",
     "initial_files": {"settings.json": "{\n  \"retries\": 2\n}\n"},
     "expected_files": {"settings.json": "{\n  \"retries\": 3\n}\n"}},
    {"id": "append_list_entry", "prompt": "Append green to the colors list.",
     "initial_files": {"colors.py": "COLORS = [\"red\", \"blue\"]\n"},
     "expected_files": {"colors.py": "COLORS = [\"red\", \"blue\", \"green\"]\n"}},
    {"id": "delete_dead_function", "prompt": "Delete only the unused dead_function definition.",
     "initial_files": {"utils.py": "def live_function():\n    return 1\n\n\ndef dead_function():\n    return 0\n"},
     "expected_files": {"utils.py": "def live_function():\n    return 1\n"}},
)
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


def _has_current_benchmark(candidate: dict[str, Any]) -> bool:
    benchmarks = candidate.get("benchmarks")
    return isinstance(benchmarks, dict) and MECHANICAL_SUITE_VERSION in benchmarks


def record_benchmark(result: dict[str, Any], *, root: Path | None = None) -> bool:
    """Persist one complete suite result without changing real-job samples."""
    model = result.get("model")
    suite_version = result.get("suite_version")
    passed, total = result.get("passed_count"), result.get("total")
    if (not isinstance(model, str) or suite_version != MECHANICAL_SUITE_VERSION
            or type(passed) is not int or type(total) is not int
            or total != len(MECHANICAL_CHORES) or not 0 <= passed <= total):
        return False
    chores = result.get("chores")
    if isinstance(chores, list) and any(
            isinstance(chore, dict) and chore.get("reason") == "infrastructure" for chore in chores):
        return False
    state = root or state_root.default_state_root()
    path = state / "opencode" / "candidates.json"
    with model_listing._candidate_lock(path) as acquired:
        if not acquired:
            return False
        path, value = _inventory(state)
        candidate = next((item for item in value["candidates"]
                          if isinstance(item, dict) and item.get("id") == model), None)
        if candidate is None:
            return False
        benchmarks = candidate.get("benchmarks")
        if not isinstance(benchmarks, dict):
            benchmarks = {}
        benchmarks[MECHANICAL_SUITE_VERSION] = {
            "passed_count": passed,
            "total": total,
            "recorded_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        }
        candidate["benchmarks"] = benchmarks
        model_listing._atomic_write(path, value)
        return True


def benchmark_unscored_candidates(*, root: Path | None = None, runner: Any = None) -> list[dict[str, Any]]:
    """Benchmark candidates missing this suite's result; completed results are never repeated."""
    state = root or state_root.default_state_root()
    path = state / "opencode" / "candidates.json"
    with model_listing._candidate_lock(path) as acquired:
        if not acquired:
            return []
        _, value = _inventory(state)
        models = [item["id"] for item in value["candidates"]
                  if isinstance(item, dict) and isinstance(item.get("id"), str)
                  and not _has_current_benchmark(item)][:MAX_BENCHMARK_CANDIDATES]
    results = []
    for model in models:
        result = run_mechanical_benchmark(model, runner=runner)
        if record_benchmark(result, root=state):
            results.append(result)
    return results


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
        flags = 0x08000000 | 0x00000200 if os.name == "nt" else 0
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


def start_benchmark(*, root: Path | None = None) -> bool:
    """Launch at most one detached, bounded inventory benchmark at a time."""
    state = root or state_root.default_state_root()
    try:
        _, value = _inventory(state)
        if not any(isinstance(item, dict) and isinstance(item.get("id"), str)
                   and not _has_current_benchmark(item) for item in value["candidates"]):
            return False
    except (OSError, ValueError, TypeError):
        return False
    lock = state / "opencode" / "benchmark.running.lock"
    try:
        descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        if not _shadow_lock_is_stale(lock):
            return False
        lock.unlink(missing_ok=True)
        try:
            descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            return False
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(str(os.getpid()))
        flags = 0x08000000 | 0x00000200 if os.name == "nt" else 0
        subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--benchmark", str(state)],
                         cwd=str(Path(__file__).resolve().parent), stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=flags, start_new_session=os.name != "nt")
        return True
    except Exception:
        lock.unlink(missing_ok=True)
        return False


def run_benchmarks(state: Path) -> None:
    lock = state / "opencode" / "benchmark.running.lock"
    try:
        benchmark_unscored_candidates(root=state)
    finally:
        lock.unlink(missing_ok=True)


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


def _run_opencode(model: str, prompt: str, scratch: Path, timeout: int,
                  runner: Any = None, *, benchmark: bool = False) -> subprocess.CompletedProcess[str]:
    import reviewer
    permissions = dict(reviewer._OPENCODE_CONFINED_PERMISSIONS)
    if benchmark:
        permissions["edit"] = permissions["write"] = "allow"
    config = {"permission": permissions,
              "agent": {"purser-shadow": {"mode": "primary", "permission": permissions}}}
    import coder_config
    executable = coder_config.opencode_binary()
    if not executable:
        raise FileNotFoundError("OpenCode executable is unavailable")
    command = [executable, "run", "--format", "json", "--model", model,
               "--agent", "purser-shadow", "--dir", str(scratch), prompt]
    environment = {**os.environ, "OPENCODE_CONFIG_CONTENT": json.dumps(config, separators=(",", ":"))}
    return (runner or subprocess.run)(command, cwd=scratch, capture_output=True, text=True,
                                      encoding="utf-8", errors="replace", timeout=timeout,
                                      check=False, env=environment,
                                      creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000)
                                      if os.name == "nt" else 0)


def _normalize_chore_text(value: str) -> str:
    return "\n".join(line.rstrip() for line in value.replace("\r\n", "\n").replace("\r", "\n").split("\n"))


def run_mechanical_benchmark(model: str, *, timeout_seconds: int = SHADOW_TIMEOUT_SECONDS,
                             runner: Any = None) -> dict[str, Any]:
    results = []
    for chore in MECHANICAL_CHORES:
        reason = "matched"
        try:
            with tempfile.TemporaryDirectory(prefix="purser-mechanical-") as directory:
                scratch = Path(directory)
                for name, content in chore["initial_files"].items():
                    path = scratch / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(content, encoding="utf-8")
                try:
                    result = _run_opencode(model, chore["prompt"], scratch, timeout_seconds, runner,
                                           benchmark=True)
                except subprocess.TimeoutExpired:
                    reason = "timeout"
                else:
                    stdout = result.stdout or ""
                    output = f"{stdout}\n{result.stderr or ''}".casefold()
                    provider_error = any(marker in output for marker in (
                        "upstream request failed", "endpoint unavailable", "endpoint is unavailable", "provider error"))
                    model_output = False
                    for line in stdout.splitlines():
                        if not line.strip():
                            continue
                        try:
                            event = json.loads(line)
                        except ValueError:
                            continue
                        part = event.get("part") if isinstance(event, dict) else None
                        if (isinstance(part, dict) and event.get("type") == "text"
                                and isinstance(part.get("text"), str) and part["text"].strip()):
                            model_output = True
                            break
                    if provider_error or (result.returncode != 0 and not model_output):
                        reason = "infrastructure"
                    elif result.returncode != 0:
                        reason = "runner_error"
                    else:
                        actual = {}
                        for path in scratch.rglob("*"):
                            if path.is_symlink():
                                reason = "out_of_scope_edit"
                                break
                            if path.is_file():
                                actual[path.relative_to(scratch).as_posix()] = path.read_text(encoding="utf-8")
                        if reason == "matched" and set(actual) != set(chore["expected_files"]):
                            reason = "out_of_scope_edit"
                        elif reason == "matched" and any(
                                _normalize_chore_text(actual[name]) != _normalize_chore_text(expected)
                                for name, expected in chore["expected_files"].items()):
                            reason = "mismatch"
        except Exception:
            reason = "runner_error"
        results.append({"id": chore["id"], "passed": reason == "matched", "reason": reason})
    passed = sum(result["passed"] for result in results)
    return {"suite_version": MECHANICAL_SUITE_VERSION, "model": model,
            "passed_count": passed, "total": len(MECHANICAL_CHORES), "chores": results}


def run_shadow(job_path: Path) -> bool:
    job = json.loads(job_path.read_text(encoding="utf-8"))
    scratch = Path(job["scratch"])
    lock = job_path.parent / "running.lock"
    success = False
    try:
        lock.write_text(str(os.getpid()), encoding="utf-8")
        source = Path(job["repository"]).resolve()
        _copy_scratch(source, scratch)
        result = _run_opencode(job["model"], job["prompt"], scratch, SHADOW_TIMEOUT_SECONDS)
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
elif __name__ == "__main__" and len(sys.argv) == 3 and sys.argv[1] == "--benchmark":
    run_benchmarks(Path(sys.argv[2]))
