"""Load and resolve the orchestrator's two-tier profile."""

from __future__ import annotations

from dataclasses import dataclass
import fnmatch
import os
import re
from pathlib import Path, PureWindowsPath
import tomllib
from collections.abc import Iterable
from typing import Any


_ENGINE_DEFAULTS = {
    "force_full": False,
    "skip_max_lines": 50,
    "skip_max_files": 2,
}

_ENGINE_PROTECTED = (
    ".orchestrator/**",
    "AGENTS.md",
    "CLAUDE.md",
    ".claude/settings.json",
    ".claude/settings.local.json",
    ".git",
    ".git/**",
    ".env",
    ".env.*",
    "*.pem",
    "*.key",
    "credentials.*",
    "secrets.*",
)


def is_protected(path: str, patterns: Iterable[str]) -> bool:
    """Return whether a repository-relative path matches a protected pattern."""

    normalized = path.replace("\\", "/")
    while normalized.startswith("./"):
        normalized = normalized[2:]
    components = tuple(part for part in normalized.split("/") if part)
    for raw_pattern in patterns:
        pattern = raw_pattern.replace("\\", "/")
        if pattern.endswith("/**"):
            prefix = pattern[:-3].rstrip("/")
            if normalized == prefix or normalized.startswith(prefix + "/"):
                return True
        elif "/" in pattern:
            if fnmatch.fnmatchcase(normalized, pattern):
                return True
        elif any(fnmatch.fnmatchcase(component.casefold(), pattern.casefold()) for component in components):
            return True
    return False

_USER_SECURITY = {
    "roles": {"orchestrator", "coder", "reviewer"},
    "reviewer": {"executable"},
    "trust": {"paths"},
    "paths": {"protected"},
    "review": {"force_full", "skip_max_lines", "skip_max_files"},
}
_USER_NON_SECURITY = {"telemetry": {"vault_root"}}
_PROJECT_SECURITY = {
    "paths": {"protected"},
    "review": {"force_full", "skip_max_lines", "skip_max_files"},
}
_DEFAULT_AUTHORITY_HEADINGS = (
    "Authority",
    "Working scope",
    "Proportional validation",
    "Test and comment quality",
)


@dataclass(frozen=True)
class AuthoritySource:
    path: str
    headings: tuple[str, ...]
    when_changed: str | None = None


@dataclass(frozen=True)
class Check:
    name: str
    argv: tuple[str, ...]
    when: tuple[str, ...] | None = None
    timeout_seconds: int = 60


@dataclass(frozen=True)
class Roles:
    orchestrator: str = "claude-code"
    coder: str = "codex"
    reviewer: str = "agy"


_PROJECT_NON_SECURITY = {"project": {"id"}, "authority": {"sources"}}
_DEFAULT_AUTHORITY_SOURCES = (
    AuthoritySource("AGENTS.md", _DEFAULT_AUTHORITY_HEADINGS),
)


@dataclass(frozen=True)
class ResolvedProfile:
    configured: bool
    project_id: str | None
    protected_paths: tuple[str, ...]
    force_full_review: bool
    skip_max_lines: int
    skip_max_files: int
    trusted: bool
    reviewer_executable: str | None
    roles: Roles
    authority_sources: tuple[AuthoritySource, ...]
    checks: tuple[Check, ...]
    vault_root: str | None
    loosenings_ignored: int
    user_status: str
    project_status: str
    warnings: tuple[str, ...]


@dataclass(frozen=True)
class _Tier:
    status: str
    values: dict[str, Any]
    warnings: tuple[str, ...]


def load_profile(
    project_root: Path, *, user_config_path: Path | None = None
) -> ResolvedProfile:
    """Return a fail-safe resolved profile without raising or writing."""

    try:
        user_path = (
            user_config_path
            if user_config_path is not None
            else Path.home() / ".orchestrator" / "config.toml"
        )
        project_path = project_root / ".orchestrator" / "profile.toml"
        user = _load_tier(user_path, "user", _USER_SECURITY, _USER_NON_SECURITY)
        project = _load_tier(
            project_path, "project", _PROJECT_SECURITY, _PROJECT_NON_SECURITY
        )
        return _resolve_profile(project_root, user, project)
    except Exception as exc:
        reason = _exception_text(exc)
        return ResolvedProfile(
            configured=False,
            project_id=None,
            protected_paths=tuple(sorted(_ENGINE_PROTECTED)),
            force_full_review=True,
            skip_max_lines=_ENGINE_DEFAULTS["skip_max_lines"],
            skip_max_files=_ENGINE_DEFAULTS["skip_max_files"],
            trusted=False,
            reviewer_executable=None,
            roles=Roles(),
            authority_sources=_DEFAULT_AUTHORITY_SOURCES,
            checks=(),
            vault_root=None,
            loosenings_ignored=0,
            user_status="rejected",
            project_status="rejected",
            warnings=(f"profile load failed: {reason}",),
        )


def _load_tier(
    path: Path,
    tier_name: str,
    security_sections: dict[str, set[str]],
    non_security_sections: dict[str, set[str]],
) -> _Tier:
    try:
        with path.open("rb") as stream:
            data = tomllib.load(stream)
    except FileNotFoundError:
        return _Tier("missing", {}, ())
    except Exception as exc:
        return _rejected(tier_name, f"could not parse profile: {_exception_text(exc)}")

    if not isinstance(data, dict):
        return _rejected(tier_name, "profile root must be a table")

    warnings: list[str] = []
    schema_version = data.get("schema_version")
    if type(schema_version) is not int or schema_version != 1:
        return _rejected(tier_name, "schema_version must be 1")

    known_sections = set(security_sections) | set(non_security_sections)
    if tier_name == "user" and "checks" in data:
        return _rejected(tier_name, "[[checks]] is project-only")
    if tier_name == "project":
        known_sections.add("checks")
    for key in data:
        if key != "schema_version" and key not in known_sections:
            warnings.append(f"{tier_name}: unknown top-level key or section [{key}] ignored")

    values: dict[str, Any] = {}
    if tier_name == "project" and "checks" in data:
        checks, reason = _parse_checks(data["checks"])
        if reason:
            return _rejected(tier_name, reason, warnings)
        values["checks"] = checks
    for section_name, allowed_keys in security_sections.items():
        if section_name not in data:
            continue
        section = data[section_name]
        if not isinstance(section, dict):
            return _rejected(
                tier_name,
                f"[{section_name}] security section must be a table",
                warnings,
            )
        for key, value in section.items():
            if key not in allowed_keys:
                return _rejected(
                    tier_name,
                    f"[{section_name}] unknown security key {key!r}",
                    warnings,
                )
            valid, reason = _valid_security_value(section_name, key, value)
            if not valid:
                return _rejected(tier_name, reason, warnings)
            values[f"{section_name}.{key}"] = value

    for section_name, allowed_keys in non_security_sections.items():
        if section_name not in data:
            continue
        section = data[section_name]
        if not isinstance(section, dict):
            warnings.append(f"{tier_name}: [{section_name}] must be a table; ignored")
            continue
        for key, value in section.items():
            if key not in allowed_keys:
                warnings.append(
                    f"{tier_name}: [{section_name}] unknown key {key!r} ignored"
                )
                continue
            if section_name == "authority" and key == "sources":
                authority_sources, authority_warnings = _parse_authority_sources(
                    tier_name, value
                )
                warnings.extend(authority_warnings)
                values["authority.sources"] = authority_sources
                continue
            if not isinstance(value, str):
                warnings.append(
                    f"{tier_name}: [{section_name}] {key} must be a string; ignored"
                )
                continue
            values[f"{section_name}.{key}"] = value

    return _Tier("ok", values, tuple(warnings))


def _parse_checks(raw: Any) -> tuple[tuple[Check, ...], str]:
    if not isinstance(raw, list) or len(raw) > 8:
        return (), "[[checks]] must be an array of at most 8 tables"
    checks: list[Check] = []
    names: set[str] = set()
    for index, entry in enumerate(raw):
        prefix = f"[[checks]] entry {index}"
        if not isinstance(entry, dict) or set(entry) - {"name", "argv", "when", "timeout_seconds"}:
            return (), f"{prefix} has invalid keys or type"
        name = entry.get("name")
        if not isinstance(name, str) or re.fullmatch(r"[a-z0-9_-]{1,32}", name) is None or name in names:
            return (), f"{prefix} name is invalid or duplicate"
        argv = entry.get("argv")
        if not isinstance(argv, list) or not argv or not all(isinstance(arg, str) and arg for arg in argv):
            return (), f"{prefix} argv must be a non-empty array of non-empty strings"
        when = entry.get("when")
        if when is not None and (not isinstance(when, list) or not all(_authority_path(pattern) is not None for pattern in when)):
            return (), f"{prefix} when must contain repository-relative globs"
        timeout = entry.get("timeout_seconds", 60)
        if type(timeout) is not int or not 1 <= timeout <= 300:
            return (), f"{prefix} timeout_seconds must be an integer from 1 to 300"
        names.add(name)
        checks.append(Check(name, tuple(argv), tuple(when) if when is not None else None, timeout))
    return tuple(checks), ""


def _parse_authority_sources(
    tier_name: str, raw_sources: Any
) -> tuple[tuple[AuthoritySource, ...], tuple[str, ...]]:
    warnings: list[str] = []
    if not isinstance(raw_sources, list):
        return (), (f"{tier_name}: [authority] sources must be a list; ignored",)

    sources: list[AuthoritySource] = []
    for index, raw_source in enumerate(raw_sources):
        prefix = f"{tier_name}: [authority] sources[{index}]"
        if not isinstance(raw_source, dict):
            warnings.append(f"{prefix} must be a table; entry ignored")
            continue
        unknown = set(raw_source).difference({"path", "headings", "when_changed"})
        if unknown:
            warnings.append(
                f"{prefix} has unknown key(s) {sorted(unknown)!r}; entry ignored"
            )
            continue

        path = raw_source.get("path")
        headings = raw_source.get("headings")
        when_changed = raw_source.get("when_changed")
        normalized_path = _authority_path(path)
        if normalized_path is None:
            warnings.append(
                f"{prefix}.path must be a relative repository path without '..'; entry ignored"
            )
            continue
        if not isinstance(headings, list) or not headings or not all(
            isinstance(heading, str) and heading for heading in headings
        ):
            warnings.append(
                f"{prefix}.headings must be a non-empty list of strings; entry ignored"
            )
            continue
        if "when_changed" in raw_source and not isinstance(when_changed, str):
            warnings.append(
                f"{prefix}.when_changed must be a string; entry ignored"
            )
            continue
        sources.append(
            AuthoritySource(
                normalized_path,
                tuple(headings),
                when_changed,
            )
        )
    return tuple(sources), tuple(warnings)


def _authority_path(value: Any) -> str | None:
    if not isinstance(value, str) or not value:
        return None
    normalized = value.replace("\\", "/")
    while normalized.startswith("./"):
        normalized = normalized[2:]
    if (
        not normalized
        or normalized.startswith("/")
        or ".." in normalized.split("/")
        or Path(value).is_absolute()
        or PureWindowsPath(value).is_absolute()
        or PureWindowsPath(value).drive
    ):
        return None
    return normalized


def _valid_security_value(section: str, key: str, value: Any) -> tuple[bool, str]:
    if section == "roles":
        allowed = {
            "orchestrator": {"claude-code"},
            "coder": {"codex", "claude"},
            "reviewer": {"agy", "none"},
        }
        if isinstance(value, str) and value in allowed[key]:
            return True, ""
        return False, f"[roles] {key} has an invalid type or value"

    if section == "review":
        if key == "force_full":
            valid = type(value) is bool
        else:
            valid = type(value) is int and value >= 0
        if not valid:
            return False, f"[review] {key} has an invalid type or value"
        return True, ""

    if section == "reviewer" and key == "executable":
        if isinstance(value, str) and os.path.isabs(value):
            return True, ""
        return False, "[reviewer] executable must be an absolute path string"

    if section == "trust" and key == "paths":
        if isinstance(value, list) and all(
            isinstance(item, str) and os.path.isabs(item) for item in value
        ):
            return True, ""
        return False, "[trust] paths must be an array of absolute path strings"

    if section == "paths" and key == "protected":
        if isinstance(value, list) and all(isinstance(item, str) for item in value):
            return True, ""
        return False, "[paths] protected must be an array of strings"

    return False, f"[{section}] {key} has an invalid type or value"


def _resolve_profile(project_root: Path, user: _Tier, project: _Tier) -> ResolvedProfile:
    baseline_force_full = _ENGINE_DEFAULTS["force_full"]
    baseline_lines = _ENGINE_DEFAULTS["skip_max_lines"]
    baseline_files = _ENGINE_DEFAULTS["skip_max_files"]

    user_values = user.values if user.status == "ok" else {}
    project_values = project.values if project.status == "ok" else {}
    baseline_force_full = user_values.get("review.force_full", baseline_force_full)
    baseline_lines = user_values.get("review.skip_max_lines", baseline_lines)
    baseline_files = user_values.get("review.skip_max_files", baseline_files)

    loosenings_ignored = 0
    skip_lines = baseline_lines
    project_lines = project_values.get("review.skip_max_lines")
    if project_lines is not None:
        if project_lines <= baseline_lines:
            skip_lines = project_lines
        else:
            loosenings_ignored += 1

    skip_files = baseline_files
    project_files = project_values.get("review.skip_max_files")
    if project_files is not None:
        if project_files <= baseline_files:
            skip_files = project_files
        else:
            loosenings_ignored += 1

    protected = set(_ENGINE_PROTECTED)
    protected.update(user_values.get("paths.protected", ()))
    protected.update(project_values.get("paths.protected", ()))

    warnings = user.warnings + project.warnings
    if user.status == "rejected" or project.status == "rejected":
        force_full = True
    else:
        force_full = bool(baseline_force_full) or bool(
            project_values.get("review.force_full", False)
        )

    trusted = _is_trusted(project_root, user)
    if "checks" in project_values and not trusted:
        warnings += ("project: [[checks]] ignored because the project is not trusted",)
    authority_sources = project_values.get("authority.sources") or _DEFAULT_AUTHORITY_SOURCES
    roles = Roles(
        orchestrator=user_values.get("roles.orchestrator", "claude-code"),
        coder=user_values.get("roles.coder", "codex"),
        reviewer=user_values.get("roles.reviewer", "agy"),
    )
    return ResolvedProfile(
        configured=project.status != "missing",
        project_id=project_values.get("project.id"),
        protected_paths=tuple(sorted(protected)),
        force_full_review=force_full,
        skip_max_lines=skip_lines,
        skip_max_files=skip_files,
        trusted=trusted,
        reviewer_executable=user_values.get("reviewer.executable"),
        roles=roles,
        authority_sources=authority_sources,
        checks=project_values.get("checks", ()) if trusted else (),
        vault_root=user_values.get("telemetry.vault_root"),
        loosenings_ignored=loosenings_ignored,
        user_status=user.status,
        project_status=project.status,
        warnings=warnings,
    )


def _is_trusted(project_root: Path, user: _Tier) -> bool:
    if user.status != "ok":
        return False
    try:
        canonical_root = os.path.normcase(str(project_root.resolve()))
    except (OSError, RuntimeError, ValueError):
        return False
    for path in user.values.get("trust.paths", ()):
        try:
            if canonical_root == os.path.normcase(str(Path(path).resolve())):
                return True
        except (OSError, RuntimeError, ValueError):
            continue
    return False


def _rejected(
    tier_name: str, reason: str, warnings: list[str] | None = None
) -> _Tier:
    messages = list(warnings or ())
    messages.append(f"{tier_name} tier rejected: {reason}")
    return _Tier("rejected", {}, tuple(messages))


def _exception_text(exc: Exception) -> str:
    text = str(exc).strip()
    return text or exc.__class__.__name__


__all__ = ["AuthoritySource", "Check", "ResolvedProfile", "is_protected", "load_profile"]
