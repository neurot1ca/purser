"""Read-only installation and usability check for the Purser Claude plugin."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

import codex_quota
import project_profile


ROOT = Path(__file__).resolve().parents[3]
STATUSES = {"OK", "WARN", "FAIL", "INFO"}


def _row(check: str, status: str, detail: str, next_step: str | None = None) -> dict:
    return {"check": check, "status": status, "detail": detail, "next_step": next_step}


def _check_python() -> dict:
    version = sys.version_info
    if version < (3, 11):
        return _row("Python", "FAIL", f"{version.major}.{version.minor}.{version.micro} at {sys.executable}; Python 3.11+ is required.", "Install Python 3.11 or newer and ensure the `python` command used by Claude resolves to it.")
    return _row("Python", "OK", f"{version.major}.{version.minor}.{version.micro}; {sys.executable}")


def _contains_manual_hook(value) -> bool:
    if isinstance(value, dict):
        return any(_contains_manual_hook(item) for item in value.values())
    if isinstance(value, list):
        return any(_contains_manual_hook(item) for item in value)
    if isinstance(value, str):
        normalized = value.replace("\\", "/").lower()
        return ".orchestrator" in normalized or "hooks/orchestrator/hook.py" in normalized
    return False


def _check_registration() -> dict:
    settings_path = Path.home() / ".claude" / "settings.json"
    try:
        settings = json.loads(settings_path.read_text(encoding="utf-8"))
        if not isinstance(settings, dict):
            raise ValueError("settings root is not an object")
    except Exception as exc:
        return _row("Hook registration", "WARN", f"Could not read {settings_path}: {exc}", "Check that ~/.claude/settings.json is readable JSON and contains the plugin or manual hook registration.")

    manual = _contains_manual_hook(settings.get("hooks", {}))
    enabled = settings.get("enabledPlugins", {})
    plugin_keys = sorted(key for key, value in enabled.items() if isinstance(key, str) and key.startswith("purser@") and value is True) if isinstance(enabled, dict) else []
    plugin_key = plugin_keys[0] if plugin_keys else None
    plugin = plugin_key is not None
    if manual and plugin:
        return _row("Hook registration", "FAIL", f"double registration: manual Purser hooks and enabled plugin `{plugin_key}` are both present.", "Remove the manual hook entries from ~/.claude/settings.json; see docs/PLUGIN.md.")
    if not manual and not plugin:
        return _row("Hook registration", "FAIL", "Neither manual Purser hooks nor an enabled Purser plugin were found.", "Install and enable `purser@<marketplace>`, or register the manual Purser hooks; see docs/PLUGIN.md.")
    if plugin:
        return _row("Hook registration", "OK", f"Purser plugin enabled as `{plugin_key}`; no manual hooks found.")
    return _row("Hook registration", "OK", "Manual Purser hooks found; no enabled Purser plugin found.")


def _check_mcp() -> dict:
    user_path = Path.home() / ".claude.json"
    settings_path = Path.home() / ".claude" / "settings.json"
    plugin_path = ROOT / "plugin" / "mcp.json"
    try:
        user_config = json.loads(user_path.read_text(encoding="utf-8")) if user_path.exists() else {}
        settings = json.loads(settings_path.read_text(encoding="utf-8")) if settings_path.exists() else {}
        plugin_config = json.loads(plugin_path.read_text(encoding="utf-8"))
        if not isinstance(user_config, dict) or not isinstance(settings, dict) or not isinstance(plugin_config, dict):
            raise ValueError("configuration root is not an object")
    except Exception as exc:
        return _row("Codex MCP", "WARN", f"Could not inspect MCP registration: {exc}", "Check that ~/.claude.json and plugin/mcp.json are readable JSON.")

    user_servers = user_config.get("mcpServers", {})
    user_server = user_servers.get("codex-main") if isinstance(user_servers, dict) else None
    plugin_servers = plugin_config.get("mcpServers", {})
    plugin_server = plugin_servers.get("codex-main") if isinstance(plugin_servers, dict) else None
    # A bundled server only starts when its plugin is enabled.
    enabled = settings.get("enabledPlugins", {})
    active_plugin = isinstance(enabled, dict) and any(
        isinstance(key, str) and key.startswith("purser@") and value is True for key, value in enabled.items()
    )
    user_active = isinstance(user_server, dict)
    plugin_active = active_plugin and isinstance(plugin_server, dict)
    user_command = user_server.get("command", "(command missing)") if user_active else None
    plugin_command = plugin_server.get("command", "(command missing)") if plugin_active else None
    if user_active and plugin_active:
        return _row("Codex MCP", "WARN", f"duplicate `codex-main`: user command `{user_command}` and enabled plugin command `{plugin_command}`.", "Remove the manual `codex-main` entry from ~/.claude.json; see docs/PLUGIN.md.")
    if user_active:
        return _row("Codex MCP", "OK", f"user-level `codex-main` command: `{user_command}`.")
    if plugin_active:
        return _row("Codex MCP", "OK", f"plugin `codex-main` command: `{plugin_command}`.")
    if isinstance(plugin_server, dict):
        return _row("Codex MCP", "INFO", "plugin `codex-main` is bundled but the Purser plugin is not enabled.")
    return _row("Codex MCP", "INFO", "No active `codex-main` MCP registration found.")


def _check_codex() -> dict:
    executable = shutil.which("codex")
    if not executable:
        return _row("Codex CLI", "FAIL", "`codex` was not found on PATH.", "Install the Codex CLI and ensure `codex` resolves on PATH.")
    try:
        result = subprocess.run([executable, "--version"], capture_output=True, text=True, timeout=10, check=False)
        output = (result.stdout or result.stderr).strip()
        if result.returncode != 0:
            return _row("Codex CLI", "WARN", f"`{executable} --version` exited {result.returncode}: {output or 'no version output'}.", "Verify the Codex CLI installation and that it can run `codex --version`.")
        match = re.search(r"(?<!\d)(\d+)\.(\d+)(?:\.(\d+))?", output)
        if not match:
            return _row("Codex CLI", "WARN", f"Could not parse version from `{output}`.", "Update the Codex CLI and verify that `codex --version` reports a version.")
        version = tuple(int(part or 0) for part in match.groups())
        if version < (0, 156, 0):
            return _row("Codex CLI", "WARN", f"{output}; gpt-6 routes require Codex CLI 0.156 or newer.", "Update the Codex CLI to version 0.156 or newer for gpt-6 routes.")
        return _row("Codex CLI", "OK", f"{output}; {executable}")
    except Exception as exc:
        return _row("Codex CLI", "WARN", f"Could not run `{executable} --version`: {exc}", "Verify that the Codex CLI executable is runnable and responds within 10 seconds.")


def _engine_config() -> dict:
    with (Path(__file__).resolve().parent / "config.json").open(encoding="utf-8") as stream:
        value = json.load(stream)
    if not isinstance(value, dict):
        raise ValueError("engine config root is not an object")
    return value


def _check_reviewer(profile=None) -> dict:
    profile = profile if profile is not None else project_profile.load_profile(Path.cwd())
    roles = getattr(profile, "roles", project_profile.Roles())
    if roles.reviewer == "none":
        return _row("Reviewer", "INFO", "No independent reviewer is configured; every call is FULL review.")
    executable = getattr(profile, "reviewer_executable", None)
    source = "user profile override" if executable else "engine config"
    if not executable:
        executable = _engine_config().get("reviewer1_executable")
    if not executable:
        return _row("Reviewer", "WARN", "No reviewer1_executable is configured; reviewer unavailable; every call will be FULL review.", "Set reviewer.executable in ~/.orchestrator/config.toml or reviewer1_executable in the engine config.")
    path = Path(executable).expanduser()
    if not path.exists():
        return _row("Reviewer", "WARN", f"{source} reviewer executable does not exist: {path}; reviewer unavailable; every call will be FULL review.", "Install or restore the configured reviewer executable, or update its configured path.")
    return _row("Reviewer", "OK", f"{source} reviewer executable exists: {path}")


def _check_roles(profile=None) -> dict:
    profile = profile if profile is not None else project_profile.load_profile(Path.cwd())
    roles = getattr(profile, "roles", project_profile.Roles())
    detail = (
        f"orchestrator={roles.orchestrator}; coder={roles.coder}; "
        f"reviewer={roles.reviewer}"
    )
    if roles.reviewer == "none":
        detail += "; every call is FULL review"
        return _row("Roles", "INFO", detail)
    return _row("Roles", "OK", detail)


def _check_project(profile=None) -> dict:
    profile = profile if profile is not None else project_profile.load_profile(Path.cwd())
    warnings = list(getattr(profile, "warnings", ()))
    detail = (f"trusted={bool(getattr(profile, 'trusted', False))}; "
              f"profile present={bool(getattr(profile, 'configured', False))}; "
              f"[[checks]]={len(getattr(profile, 'checks', ())) }")
    if warnings:
        detail += "; warnings: " + " | ".join(str(item) for item in warnings)
        return _row("Project", "WARN", detail, "Review the listed profile warnings and correct .orchestrator/profile.toml or ~/.orchestrator/config.toml.")
    return _row("Project", "OK", detail)


def _newest_rollout_thread() -> str | None:
    sessions = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex") / "sessions"
    try:
        latest = max(sessions.rglob("rollout-*.jsonl"), key=lambda path: path.stat().st_mtime, default=None)
        if latest is None:
            return None
        match = re.search(r"([0-9a-fA-F]{8}-[0-9a-fA-F-]{27,})\.jsonl$", latest.name)
        return match.group(1) if match else None
    except OSError:
        return None


def _check_quota() -> dict:
    try:
        thread_id = _newest_rollout_thread()
        quota = codex_quota.read_quota(thread_id) if thread_id else None
        if not quota:
            return _row("Quota", "INFO", "Quota data is unavailable for the newest rollout.")
        values = []
        for label, key in (("5h", "5h_used_pct"), ("7d", "7d_used_pct")):
            if key in quota:
                values.append(f"{label} used {quota[key]}%")
        if not values:
            return _row("Quota", "INFO", "Newest rollout has no 5h/7d quota values.")
        return _row("Quota", "OK", "; ".join(values))
    except Exception as exc:
        return _row("Quota", "INFO", f"Quota data is unavailable: {exc}")


def _run_check(label, check) -> dict:
    try:
        result = check()
        if not isinstance(result, dict) or result.get("status") not in STATUSES:
            raise ValueError("check returned an invalid result")
        return result
    except Exception as exc:
        return _row(label, "WARN", f"Check could not run: {exc}", "Resolve the reported check error and run `/purser:check` again.")


def collect_report() -> dict:
    checks = [
        _run_check("Python", _check_python),
        _run_check("Hook registration", _check_registration),
        _run_check("Codex MCP", _check_mcp),
        _run_check("Codex CLI", _check_codex),
    ]
    profile = None
    try:
        profile = project_profile.load_profile(Path.cwd())
    except Exception:
        pass
    checks.extend([
        _run_check("Roles", lambda: _check_roles(profile)),
        _run_check("Reviewer", lambda: _check_reviewer(profile)),
        _run_check("Project", lambda: _check_project(profile)),
        _run_check("Quota", _check_quota),
    ])
    next_steps = [
        {"number": index, "check": row["check"], "action": row["next_step"]}
        for index, row in enumerate((item for item in checks if item["status"] in {"WARN", "FAIL"}), 1)
    ]
    return {"checks": checks, "next_steps": next_steps, "exit_code": int(any(row["status"] == "FAIL" for row in checks))}


def render_markdown(report: dict) -> str:
    lines = ["| Check | Status | Detail |", "| --- | --- | --- |"]
    for row in report["checks"]:
        detail = str(row["detail"]).replace("|", "\\|").replace("\n", " ")
        lines.append(f"| {row['check']} | {row['status']} | {detail} |")
    if report["next_steps"]:
        lines.extend(("", "Next steps:"))
        for step in report["next_steps"]:
            lines.append(f"{step['number']}. **{step['check']}:** {step['action']}")
    return "\n".join(lines)


def main(argv=None) -> int:
    try:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("--json", action="store_true", help="emit machine-readable JSON")
        args = parser.parse_args(argv)
        report = collect_report()
        if args.json:
            print(json.dumps(report, ensure_ascii=False))
        else:
            print(render_markdown(report))
        return report["exit_code"]
    except Exception as exc:
        fallback = {
            "checks": [_row("Purser check", "WARN", f"Check could not run: {exc}", "Resolve this error and run `/purser:check` again.")],
            "next_steps": [{"number": 1, "check": "Purser check", "action": "Resolve this error and run `/purser:check` again."}],
            "exit_code": 0,
        }
        try:
            print(json.dumps(fallback, ensure_ascii=False) if "--json" in (argv or sys.argv[1:]) else render_markdown(fallback))
        except Exception:
            print("| Check | Status | Detail |\n| --- | --- | --- |\n| Purser check | WARN | check could not run |\n\nNext steps:\n1. Resolve the check error and run `/purser:check` again.")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
