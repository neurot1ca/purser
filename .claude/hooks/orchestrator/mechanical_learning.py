"""Fail-open sampling and ranking for free mechanical OpenCode models."""

from __future__ import annotations

import json
import errno
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
BENCHMARK_LOCK_GRACE_SECONDS = 30
BENCHMARK_LOGS_TO_KEEP = 5
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


def _benchmark_lock_is_stale(lock: Path) -> bool:
    try:
        value = json.loads(lock.read_text(encoding="utf-8"))
        pid, heartbeat = value["pid"], value["heartbeat"]
        stale = time.time() - float(heartbeat) > SHADOW_TIMEOUT_SECONDS + BENCHMARK_LOCK_GRACE_SECONDS
    except (OSError, ValueError, TypeError, KeyError):
        try:
            pid = int(lock.read_text(encoding="utf-8").strip())
        except (OSError, ValueError):
            pid = 0
        try:
            stale = time.time() - lock.stat().st_mtime > SHADOW_TIMEOUT_SECONDS + BENCHMARK_LOCK_GRACE_SECONDS
        except OSError:
            return False
    if not stale:
        return False
    if os.name == "nt":
        return True
    if type(pid) is not int or pid <= 0:
        return True
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return True
    except PermissionError:
        return False
    except OSError as exc:
        return exc.errno == errno.ESRCH or getattr(exc, "winerror", None) == 87
    return False


def _write_benchmark_lock(lock: Path, pid: int, run_id: str | None = None) -> None:
    value = {"pid": pid, "heartbeat": time.time()}
    if run_id is not None:
        value["run_id"] = run_id
    model_listing._atomic_write(lock, value)


def _record_interrupted_run(lock: Path) -> None:
    try:
        value = json.loads(lock.read_text(encoding="utf-8"))
        run_id = value.get("run_id")
        if isinstance(run_id, str):
            events = lock.parent / f"benchmark-{run_id}.jsonl"
            rows = events.read_text(encoding="utf-8").splitlines() if events.exists() else []
            if rows and json.loads(rows[-1]).get("event") not in {"completed", "failed", "interrupted"}:
                _append_benchmark_event(events, "interrupted", run_id=run_id,
                                        reason="worker heartbeat expired and process exited")
    except (OSError, ValueError, TypeError):
        pass


def _append_benchmark_event(path: Path, event: str, **fields: Any) -> None:
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps({"event": event, **fields}, ensure_ascii=False, separators=(",", ":")) + "\n")


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
        import coder_config
        before = coder_config._ordered_opencode_candidates(value["candidates"])
        benchmarks = candidate.get("benchmarks")
        if not isinstance(benchmarks, dict):
            benchmarks = {}
        benchmarks[MECHANICAL_SUITE_VERSION] = {
            "passed_count": passed,
            "total": total,
            "recorded_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        }
        candidate["benchmarks"] = benchmarks
        after = coder_config._ordered_opencode_candidates(value["candidates"])
        model_listing._atomic_write(path, value)
        if before[:1] != after[:1] and after and after[0] == model and candidate.get("status") != "measured":
            try:
                if passed >= BENCHMARK_MIN_PASSED:
                    leader_path = state / "opencode" / "leader.json"
                    model_listing._atomic_write(leader_path, {
                        "model": model,
                        "successes": passed,
                        "sample_count": total,
                        "announced": False,
                    })
            except BaseException:
                pass
        return True


def benchmark_unscored_candidates(*, root: Path | None = None, runner: Any = None,
                                  event_log: Path | None = None) -> list[dict[str, Any]]:
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
        result = run_mechanical_benchmark(model, runner=runner,
                                          heartbeat=lambda: _write_benchmark_lock(
                                              state / "opencode" / "benchmark.running.lock", os.getpid(),
                                              event_log.stem.removeprefix("benchmark-") if event_log else None))
        recorded = record_benchmark(result, root=state)
        if event_log is not None:
            _append_benchmark_event(event_log, "candidate", model=model, recorded=recorded,
                                    passed_count=result.get("passed_count"), total=result.get("total"),
                                    chores=result.get("chores", []))
        if recorded:
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
            lock = state / "opencode" / "benchmark.running.lock"
            if lock.exists() and _benchmark_lock_is_stale(lock):
                _record_interrupted_run(lock)
                lock.unlink(missing_ok=True)
            return False
    except (OSError, ValueError, TypeError):
        return False
    lock = state / "opencode" / "benchmark.running.lock"
    try:
        descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        if not _benchmark_lock_is_stale(lock):
            return False
        _record_interrupted_run(lock)
        lock.unlink(missing_ok=True)
        try:
            descriptor = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            return False
    try:
        run_id = uuid.uuid4().hex
        events = state / "opencode" / f"benchmark-{run_id}.jsonl"
        output = state / "opencode" / f"benchmark-{run_id}.log"
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump({"pid": os.getpid(), "heartbeat": time.time(), "run_id": run_id}, handle)
        events.touch()
        _append_benchmark_event(events, "start", run_id=run_id, pid=os.getpid())
        flags = 0x08000000 | 0x00000200 if os.name == "nt" else 0
        with output.open("ab") as log:
            launched = subprocess.Popen([sys.executable, str(Path(__file__).resolve()),
                                         "--benchmark", str(state), run_id],
                                        cwd=str(Path(__file__).resolve().parent), stdin=subprocess.DEVNULL,
                                        stdout=log, stderr=subprocess.STDOUT,
                                        creationflags=flags, start_new_session=os.name != "nt")
        _write_benchmark_lock(lock, launched.pid, run_id)
        logs = sorted((state / "opencode").glob("benchmark-*.jsonl"),
                      key=lambda path: path.stat().st_mtime, reverse=True)
        for old in logs[BENCHMARK_LOGS_TO_KEEP:]:
            old.unlink(missing_ok=True)
            old.with_suffix(".log").unlink(missing_ok=True)
        return True
    except Exception:
        if "events" in locals():
            _append_benchmark_event(events, "failed", run_id=run_id, reason="worker launch failed")
        lock.unlink(missing_ok=True)
        return False


def run_benchmarks(state: Path, run_id: str | None = None, *, runner: Any = None) -> None:
    lock = state / "opencode" / "benchmark.running.lock"
    run_id = run_id or uuid.uuid4().hex
    events = state / "opencode" / f"benchmark-{run_id}.jsonl"
    try:
        _write_benchmark_lock(lock, os.getpid(), run_id)
        if not events.exists() or events.stat().st_size == 0:
            _append_benchmark_event(events, "start", run_id=run_id, pid=os.getpid())
        while True:
            results = benchmark_unscored_candidates(root=state, runner=runner, event_log=events)
            try:
                _, inventory = _inventory(state)
                remaining = any(isinstance(item, dict) and isinstance(item.get("id"), str)
                                and not _has_current_benchmark(item) for item in inventory["candidates"])
            except (OSError, ValueError, TypeError):
                remaining = False
            if not remaining:
                break
            if not results:
                raise RuntimeError("benchmark batch made no scoring progress")
        _append_benchmark_event(events, "completed", run_id=run_id, reason="no_unscored_candidates")
    except BaseException as exc:
        _append_benchmark_event(events, "failed", run_id=run_id,
                                reason=f"{type(exc).__name__}: {exc}")
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


def _opencode_tokens(stdout: str) -> dict[str, int]:
    totals: dict[str, int] = {}
    fields = {"input_tokens": ("input", "input_tokens"),
              "output_tokens": ("output", "output_tokens"),
              "total_tokens": ("total", "total_tokens")}
    for line in stdout.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        part = event.get("part") if isinstance(event, dict) else None
        usage = part.get("tokens") if isinstance(part, dict) else None
        if event.get("type") == "step_finish" and isinstance(usage, dict):
            for target, sources in fields.items():
                value = next((usage[key] for key in sources if key in usage), None)
                if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                    totals[target] = totals.get(target, 0) + value
    return totals


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
                             runner: Any = None, heartbeat: Any = None) -> dict[str, Any]:
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
                    if heartbeat is not None:
                        heartbeat()
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
                finally:
                    if heartbeat is not None:
                        heartbeat()
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
    completed = False
    agreement = None
    try:
        lock.write_text(str(os.getpid()), encoding="utf-8")
        source = Path(job["repository"]).resolve()
        _copy_scratch(source, scratch)
        result = _run_opencode(job["model"], job["prompt"], scratch, SHADOW_TIMEOUT_SECONDS)
        if result.returncode == 0:
            report = _final_report(result.stdout)
            completed = True
            agreement = reports_agree(report, job["accepted_report"])
            success = agreement
    except Exception:
        success = False
    try:
        root = Path(job["state_root"])
        record_sample(job["model"], success, root=root)
    except Exception:
        pass
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
        job_path.unlink(missing_ok=True)
        lock.unlink(missing_ok=True)
    return success


if __name__ == "__main__" and len(sys.argv) == 3 and sys.argv[1] == "--shadow":
    run_shadow(Path(sys.argv[2]))
elif __name__ == "__main__" and len(sys.argv) == 4 and sys.argv[1] == "--benchmark":
    run_benchmarks(Path(sys.argv[2]), sys.argv[3])
