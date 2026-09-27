"""Bounded Antigravity Reviewer-1 transport."""

from __future__ import annotations

from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass
import json
import math
import os
from pathlib import Path, PureWindowsPath
import re
import shutil
import subprocess
import tempfile
import time
from typing import Any
import uuid

import ledger
import policy


_PACKAGE_ROOT = Path(__file__).resolve().parent
_CONFIG_PATH = _PACKAGE_ROOT / "config.json"
_PACKET_FIELDS = (
    "objective",
    "acceptance_criteria",
    "authority_contract",
    "delegated_diff",
    "validation_summary",
)
_COMPLETED_RESULTS = frozenset(
    {
        policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        policy.CHANGES_REQUIRED,
        policy.UNCERTAIN,
    }
)
_ALL_RESULTS = frozenset({*_COMPLETED_RESULTS, policy.REVIEWER1_UNAVAILABLE})
_ENVELOPE_FIELDS = frozenset(
    {
        "conversation_id",
        "status",
        "response",
        "duration_seconds",
        "num_turns",
        "usage",
    }
)
_FORBIDDEN_TEXT = (
    re.compile(r"(?im)^\s*(?:implementer narrative|hidden reasoning|claude conversation|"
               r"full success logs?|whole[- ]repository state|raw agent output)\s*:"),
    re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    re.compile(r"\b(?:sk[-_]|ghp_|github_pat_)[A-Za-z0-9_-]{16,}\b"),
    re.compile(r"\bAKIA[A-Z0-9]{16}\b"),
    re.compile(r"\bBearer\s+[A-Za-z0-9._~+/-]{12,}", re.IGNORECASE),
    re.compile(
        r"(?i)\b(?:api[_-]?key|access[_-]?token|client[_-]?secret|private[_-]?key|password)"
        r"\s*[:=]\s*['\"]?[A-Za-z0-9_./+\-=]{12,}"
    ),
)
_FAST_PROCESS_FAILURE_RETRY_SECONDS = 60.0
_MIN_RETRY_REMAINING_SECONDS = 5.0
_NARRATIVE_TEXT = _FORBIDDEN_TEXT[:1]
_AUTHORITY_HEADINGS = (
    "Authority",
    "Working scope",
    "Proportional validation",
    "Test and comment quality",
)
_BUILD_SPEC_HEADINGS = (
    "Corrections to the canonical spec",
    "Reviewer-1 gate (mechanical)",
    "Hard constraints",
)
_DIFF_REDACTION_MARKER = "[[REDACTED: PRE-EXISTING OR REMOVED SENSITIVE SPAN]]"


class _AgyAdapter:
    provider = "agy"
    allowed_model = "gemini-3.8-flash-high"

    def validate_model(self, model: Any) -> None:
        if model != self.allowed_model:
            raise ValueError(
                f"reviewer1_model_requested must equal the pinned id {self.allowed_model}"
            )

    def build_command(
        self,
        prompt: str,
        schema: Mapping[str, Any],
        config: Mapping[str, Any],
        *,
        add_dirs: Sequence[str | os.PathLike[str]] = (),
        extra_flags: Sequence[str] = (),
        repository_root: Path | None = None,
    ) -> list[str]:
        executable = config.get("reviewer1_executable")
        model = config.get("reviewer1_model_requested")
        if not isinstance(executable, str) or not os.path.isabs(executable):
            raise ValueError("reviewer1_executable must be an absolute path")
        self.validate_model(model)
        command = [
            executable,
            "--print",
            prompt,
            "--output-format",
            "json",
            "--json-schema",
            _canonical_json(schema).decode("utf-8"),
            "--model",
            model,
            "--mode",
            "plan",
            "--sandbox",
        ]
        for directory in add_dirs:
            command.extend(("--add-dir", str(Path(directory).resolve())))
        command.extend(extra_flags)
        refuse_unsafe_flag_combination(command, repository_root=repository_root)
        return command

    def invoke(
        self,
        command: Sequence[str],
        *,
        runner: Callable[..., subprocess.CompletedProcess[str]],
        cwd: Path,
        timeout: float,
    ) -> subprocess.CompletedProcess[str]:
        kwargs: dict[str, Any] = {
            "cwd": cwd,
            "capture_output": True,
            "text": True,
            "encoding": "utf-8",
            "errors": "replace",
            "timeout": timeout,
            "check": False,
        }
        if os.name == "nt":
            kwargs["creationflags"] = 0x08000000  # CREATE_NO_WINDOW
        return runner(
            command,
            **kwargs,
        )

    def parse(
        self,
        stdout: str,
        *,
        include_metadata: bool = False,
    ) -> tuple[str, dict[str, Any]] | tuple[str, dict[str, Any], float, dict[str, int | float]]:
        return _parse_agy_envelope(stdout, include_metadata=include_metadata)


AGY_ADAPTER = _AgyAdapter()
PINNED_MODEL = AGY_ADAPTER.allowed_model


_OPENCODE_DENIED_PERMISSIONS = {
    "edit": "deny",
    "write": "deny",
    "bash": "deny",
    "webfetch": "deny",
    "task": "deny",
    "external_directory": "deny",
}


class _OpenCodeAdapter:
    provider = "opencode"

    def validate_model(self, model: Any) -> None:
        if (
            not isinstance(model, str)
            or model.count("/") != 1
            or not all(model.split("/"))
        ):
            raise ValueError("OpenCode reviewer model must be provider/model")

    def build_command(
        self,
        prompt: str,
        schema: Mapping[str, Any],
        config: Mapping[str, Any],
        *,
        cwd: Path,
    ) -> list[str]:
        executable = config.get("reviewer1_executable")
        model = config.get("reviewer1_model_requested")
        agent = config.get("reviewer1_agent", "reviewer")
        if not isinstance(executable, str) or not os.path.isabs(executable):
            raise ValueError("reviewer1_executable must be an absolute path")
        self.validate_model(model)
        if not isinstance(agent, str) or not agent.strip():
            raise ValueError("reviewer1_agent must be a nonempty name")
        return [
            executable,
            "run",
            "--format",
            "json",
            "--model",
            model,
            "--agent",
            agent,
            "--dir",
            str(cwd.resolve()),
            prompt,
        ]

    def invoke(
        self,
        command: Sequence[str],
        *,
        runner: Callable[..., subprocess.CompletedProcess[str]],
        cwd: Path,
        timeout: float,
    ) -> subprocess.CompletedProcess[str]:
        agent = command[command.index("--agent") + 1]
        permissions = {
            "permission": dict(_OPENCODE_DENIED_PERMISSIONS),
            "agent": {
                agent: {"permission": dict(_OPENCODE_DENIED_PERMISSIONS)}
            },
        }
        kwargs: dict[str, Any] = {
            "cwd": cwd,
            "capture_output": True,
            "text": True,
            "encoding": "utf-8",
            "errors": "replace",
            "timeout": timeout,
            "check": False,
            "env": {
                **os.environ,
                "OPENCODE_CONFIG_CONTENT": _canonical_json(permissions).decode("utf-8"),
            },
        }
        if os.name == "nt":
            kwargs["creationflags"] = 0x08000000  # CREATE_NO_WINDOW
        return runner(command, **kwargs)

    def parse(
        self,
        stdout: str,
        *,
        include_metadata: bool = False,
    ) -> tuple[str, dict[str, Any], float | None, dict[str, int | float], str]:
        return _parse_event_stream(stdout, self.provider)


class _CodexAdapter:
    provider = "codex"

    def validate_model(self, model: Any) -> None:
        if model is not None and (not isinstance(model, str) or not model.strip()):
            raise ValueError("Codex reviewer model must be a nonempty string")

    def build_command(
        self,
        prompt: str,
        schema: Mapping[str, Any],
        config: Mapping[str, Any],
        *,
        cwd: Path,
    ) -> list[str]:
        executable = config.get("reviewer1_executable")
        model = config.get("reviewer1_model_requested")
        if not isinstance(executable, str) or not os.path.isabs(executable):
            raise ValueError("reviewer1_executable must be an absolute path")
        self.validate_model(model)
        command = [
            executable,
            "exec",
            "--json",
            "--skip-git-repo-check",
            "-s",
            "read-only",
            "-c",
            'approval_policy="never"',
            "-C",
            str(cwd.resolve()),
        ]
        if model:
            command.extend(("-m", model))
        return [*command, prompt]

    def invoke(
        self,
        command: Sequence[str],
        *,
        runner: Callable[..., subprocess.CompletedProcess[str]],
        cwd: Path,
        timeout: float,
    ) -> subprocess.CompletedProcess[str]:
        kwargs: dict[str, Any] = {
            "cwd": cwd,
            "capture_output": True,
            "text": True,
            "encoding": "utf-8",
            "errors": "replace",
            "timeout": timeout,
            "check": False,
        }
        if os.name == "nt":
            kwargs["creationflags"] = 0x08000000  # CREATE_NO_WINDOW
        return runner(command, **kwargs)

    def parse(
        self,
        stdout: str,
        *,
        include_metadata: bool = False,
    ) -> tuple[str, dict[str, Any], float | None, dict[str, int | float], str]:
        return _parse_event_stream(stdout, self.provider)


OPENCODE_ADAPTER = _OpenCodeAdapter()
CODEX_ADAPTER = _CodexAdapter()
REVIEWER_ADAPTERS: dict[str, Any] = {
    AGY_ADAPTER.provider: AGY_ADAPTER,
    OPENCODE_ADAPTER.provider: OPENCODE_ADAPTER,
    CODEX_ADAPTER.provider: CODEX_ADAPTER,
}


def register_reviewer_adapter(agent: str, adapter: Any) -> None:
    """Register an agent-specific transport/parser without changing selection."""
    if not isinstance(agent, str) or not agent or not all(
        callable(getattr(adapter, name, None)) for name in ("build_command", "invoke", "parse", "validate_model")
    ):
        raise ValueError("reviewer adapter registration is invalid")
    REVIEWER_ADAPTERS[agent] = adapter


REVIEW_SCHEMA: dict[str, Any] = {
    "type": "object",
    "additionalProperties": False,
    "required": ["result", "validation", "findings"],
    "properties": {
        "result": {"type": "string", "enum": sorted(_ALL_RESULTS)},
        "validation": {"type": "string", "enum": ["PASS", "FAIL", "UNKNOWN"]},
        "findings": {
            "type": "array",
            "maxItems": 20,
            "items": {
                "type": "object",
                "additionalProperties": False,
                "required": ["code", "message"],
                "properties": {
                    "code": {"type": "string", "minLength": 1, "maxLength": 120},
                    "message": {"type": "string", "minLength": 1, "maxLength": 2000},
                },
            },
        },
    },
}


class CommandRefused(ValueError):
    """The requested command would remove the read-only guarantee."""


@dataclass(frozen=True)
class PacketReady:
    packet: Mapping[str, str]
    encoded: bytes
    added_redaction_count: int = 0
    added_affected_files: tuple[str, ...] = ()


@dataclass(frozen=True)
class PacketOmitted:
    code: str
    message: str
    omitted: tuple[str, ...]


PacketBuild = PacketReady | PacketOmitted


class _ResultMapping(Mapping[str, Any]):
    def _values(self) -> dict[str, Any]:
        raise NotImplementedError

    def __getitem__(self, key: str) -> Any:
        return self._values()[key]

    def __iter__(self) -> Iterator[str]:
        return iter(self._values())

    def __len__(self) -> int:
        return len(self._values())


@dataclass(frozen=True)
class CompletedReview(_ResultMapping):
    """A review that ran successfully; no transport-failure fields exist."""

    result: str
    validation: str
    findings: tuple[tuple[str, str], ...]
    conversation_id: str
    reviewer1_model_requested: str
    duration_seconds: float | None = None
    usage: tuple[tuple[str, int | float], ...] = ()
    provider: str = AGY_ADAPTER.provider
    served_model_attestation: str = "UNAVAILABLE"

    def __post_init__(self) -> None:
        if self.result not in _COMPLETED_RESULTS:
            raise ValueError("a completed review cannot carry a failure result")

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "COMPLETED",
            "available": True,
            "provider": self.provider,
            "result": self.result,
            "validation": self.validation,
            "findings": [
                {"code": code, "message": message} for code, message in self.findings
            ],
            "conversation_id": self.conversation_id,
            "reviewer1_model_requested": self.reviewer1_model_requested,
            "served_model_attestation": self.served_model_attestation,
        }


@dataclass(frozen=True)
class UnavailableReview(_ResultMapping):
    """A failed invocation; it cannot carry a passing reviewer result."""

    error_code: str
    message: str
    reviewer1_model_requested: str
    duration_seconds: float | None = None
    usage: tuple[tuple[str, int | float], ...] = ()
    raw_stdout: str | None = None
    decoder_detail: str | None = None
    elapsed_seconds: float | None = None
    stderr_tail: str | None = None
    provider: str = AGY_ADAPTER.provider

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "UNAVAILABLE",
            "available": False,
            "provider": self.provider,
            "result": policy.REVIEWER1_UNAVAILABLE,
            "validation": "UNKNOWN",
            "findings": [_finding(self.error_code, self.message)],
            "error_code": self.error_code,
            "reviewer1_model_requested": self.reviewer1_model_requested,
            "served_model_attestation": "UNAVAILABLE",
        }


@dataclass(frozen=True)
class IncompleteReview(_ResultMapping):
    """No invocation occurred because complete review material could not be sent."""

    code: str
    message: str
    omitted: tuple[str, ...]
    reviewer1_model_requested: str
    provider: str = AGY_ADAPTER.provider

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "NOT_RUN_INCOMPLETE_PACKET",
            "available": True,
            "provider": self.provider,
            "result": policy.UNCERTAIN,
            "validation": "UNKNOWN",
            "findings": [_finding(self.code, self.message)],
            "omitted": list(self.omitted),
            "reviewer1_model_requested": self.reviewer1_model_requested,
            "served_model_attestation": "UNAVAILABLE",
        }


ReviewOutcome = CompletedReview | UnavailableReview | IncompleteReview


def _finding(code: str, message: str) -> dict[str, str]:
    return {"code": code, "message": message}


def _requested_model(config: Mapping[str, Any]) -> str:
    requested = config.get("reviewer1_model_requested")
    return requested if isinstance(requested, str) and requested else "UNAVAILABLE"


def _completed(
    result: str,
    validation: str,
    findings: list[dict[str, str]],
    conversation_id: str,
    config: Mapping[str, Any],
    duration_seconds: float | None = None,
    usage: Mapping[str, int | float] | None = None,
    *,
    provider: str = AGY_ADAPTER.provider,
    served_model_attestation: str = "UNAVAILABLE",
) -> CompletedReview:
    return CompletedReview(
        result=result,
        validation=validation,
        findings=tuple((item["code"], item["message"]) for item in findings),
        conversation_id=conversation_id,
        reviewer1_model_requested=_requested_model(config),
        duration_seconds=duration_seconds,
        usage=tuple(sorted((usage or {}).items())),
        provider=provider,
        served_model_attestation=served_model_attestation,
    )


def _unavailable(
    code: str,
    message: str,
    config: Mapping[str, Any],
    *,
    provider: str = AGY_ADAPTER.provider,
) -> UnavailableReview:
    return UnavailableReview(
        code, message, _requested_model(config), provider=provider
    )


def _stderr_tail(value: Any) -> str | None:
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    return value[-2048:] if isinstance(value, str) and value else None


def _incomplete_packet(
    packet: PacketOmitted,
    config: Mapping[str, Any],
    *,
    provider: str = AGY_ADAPTER.provider,
) -> IncompleteReview:
    return IncompleteReview(
        packet.code,
        packet.message,
        packet.omitted,
        _requested_model(config),
        provider=provider,
    )


def _canonical_json(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")


def _contains_forbidden_text(value: str) -> bool:
    return any(pattern.search(value) for pattern in _FORBIDDEN_TEXT)


def _contains_excluded_narrative(value: str) -> bool:
    return any(pattern.search(value) for pattern in _NARRATIVE_TEXT)


def _forbidden_spans(value: str) -> tuple[tuple[int, int], ...]:
    spans = sorted(
        (match.start(), match.end())
        for pattern in _FORBIDDEN_TEXT
        for match in pattern.finditer(value)
    )
    merged: list[tuple[int, int]] = []
    for start, end in spans:
        if merged and start < merged[-1][1]:
            merged[-1] = (merged[-1][0], max(end, merged[-1][1]))
        else:
            merged.append((start, end))
    return tuple(merged)


def _diff_header_path(value: str) -> str | None:
    if value == "/dev/null":
        return None
    return value[2:] if value.startswith(("a/", "b/")) else value


def _redact_delegated_diff(
    diff: str,
) -> tuple[str, int, tuple[str, ...], int, tuple[str, ...]] | None:
    redacted_lines: list[str] = []
    affected_files: set[str] = set()
    added_affected_files: set[str] = set()
    redaction_count = 0
    added_redaction_count = 0
    old_path: str | None = None
    current_path: str | None = None
    in_hunk = False

    for raw_line in diff.splitlines(keepends=True):
        if raw_line.endswith("\r\n"):
            line, ending = raw_line[:-2], "\r\n"
        elif raw_line.endswith(("\n", "\r")):
            line, ending = raw_line[:-1], raw_line[-1]
        else:
            line, ending = raw_line, ""

        line_kind: str | None = None
        content = line
        if line.startswith("diff --git "):
            old_path = None
            current_path = None
            in_hunk = False
        elif not in_hunk and line.startswith("--- "):
            old_path = _diff_header_path(line[4:])
        elif not in_hunk and line.startswith("+++ "):
            current_path = _diff_header_path(line[4:]) or old_path
        elif line.startswith("@@ "):
            in_hunk = True
        elif in_hunk and line.startswith(("+", "-", " ")):
            line_kind = {"+": "added", "-": "removed", " ": "context"}[line[0]]
            content = line[1:]

        spans = _forbidden_spans(content)
        if not spans:
            redacted_lines.append(raw_line)
            continue
        if _contains_excluded_narrative(content):
            return None
        if line_kind is None or current_path is None:
            return None

        pieces: list[str] = []
        cursor = 0
        for start, end in spans:
            pieces.extend((content[cursor:start], _DIFF_REDACTION_MARKER))
            cursor = end
        pieces.append(content[cursor:])
        redacted_lines.append(line[0] + "".join(pieces) + ending)
        redaction_count += len(spans)
        affected_files.add(current_path)
        if line_kind == "added":
            added_redaction_count += len(spans)
            added_affected_files.add(current_path)

    return (
        "".join(redacted_lines),
        redaction_count,
        tuple(sorted(affected_files)),
        added_redaction_count,
        tuple(sorted(added_affected_files)),
    )


def build_review_packet(materials: Mapping[str, Any], max_bytes: int) -> PacketBuild:
    """Build the exact five-field packet, rejecting extras and unsafe content."""

    if not isinstance(materials, Mapping):
        return PacketOmitted(
            "REVIEW_PACKET_INVALID",
            "The review packet source was not a mapping; no context was sent.",
            tuple(_PACKET_FIELDS),
        )
    extras = sorted(set(materials).difference(_PACKET_FIELDS))
    missing = [field for field in _PACKET_FIELDS if field not in materials]
    if extras or missing:
        details = []
        if extras:
            details.append(f"forbidden or unrelated fields: {', '.join(extras)}")
        if missing:
            details.append(f"missing required fields: {', '.join(missing)}")
        return PacketOmitted(
            "REVIEW_PACKET_FIELDS_REJECTED",
            f"The packet allowlist rejected {'; '.join(details)}; no context was sent.",
            tuple(missing or extras),
        )
    if not isinstance(max_bytes, int) or isinstance(max_bytes, bool) or max_bytes <= 0:
        return PacketOmitted(
            "REVIEW_PACKET_BUDGET_INVALID",
            "The packet byte budget was invalid; no context was sent.",
            tuple(_PACKET_FIELDS),
        )

    packet: dict[str, str] = {}
    redaction_count = 0
    affected_files: tuple[str, ...] = ()
    added_redaction_count = 0
    added_affected_files: tuple[str, ...] = ()
    for field in _PACKET_FIELDS:
        value = materials[field]
        if not isinstance(value, str) or not value.strip():
            return PacketOmitted(
                "REVIEW_PACKET_VALUE_INVALID",
                f"The required packet field {field!r} was empty or non-text; no context was sent.",
                (field,),
            )
        if field == "delegated_diff":
            redacted = _redact_delegated_diff(value)
            if redacted is None:
                return PacketOmitted(
                    "REVIEW_PACKET_SENSITIVE_CONTENT",
                    "Credential-like or excluded narrative material was detected in "
                    f"{field!r}; it was not sent.",
                    (field,),
                )
            (
                value,
                redaction_count,
                affected_files,
                added_redaction_count,
                added_affected_files,
            ) = redacted
        if _contains_forbidden_text(value):
            return PacketOmitted(
                "REVIEW_PACKET_SENSITIVE_CONTENT",
                f"Credential-like or excluded narrative material was detected in {field!r}; it was not sent.",
                (field,),
            )
        packet[field] = value

    if redaction_count:
        files = ", ".join(affected_files)
        material_description = (
            "added credential-shaped material"
            if added_redaction_count
            else "pre-existing context or removed credential-shaped/excluded material"
        )
        packet["validation_summary"] += (
            f"\n\nREDACTION NOTICE: {redaction_count} forbidden-text span(s) were redacted "
            f"from {material_description} in: "
            f"{files}. Each unmistakable {_DIFF_REDACTION_MARKER} marker replaces the complete "
            "matched span. A redaction is not permission to ignore a possible defect; question "
            "any unexplained redaction."
        )

    encoded = _canonical_json(packet)
    if len(encoded) > max_bytes:
        diff_bytes = len(materials["delegated_diff"].encode("utf-8"))
        if diff_bytes > max_bytes:
            omitted = ("delegated_diff (complete diff exceeds packet budget; not truncated)",)
        else:
            omitted = tuple(_PACKET_FIELDS)
        return PacketOmitted(
            "REVIEW_PACKET_TOO_LARGE",
            (
                f"The complete {len(encoded)}-byte packet exceeds the {max_bytes}-byte budget. "
                f"Omitted: {', '.join(omitted)}. No partial diff was sent and no model call was made."
            ),
            omitted,
        )
    return PacketReady(
        packet,
        encoded,
        added_redaction_count,
        added_affected_files,
    )


def _has_flag(command: Sequence[str], name: str, value: str | None = None) -> bool:
    for index, item in enumerate(command):
        if item == name:
            return value is None or (index + 1 < len(command) and command[index + 1] == value)
        if item.startswith(name + "="):
            return value is None or item == f"{name}={value}"
    return False


def _within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


def _refuse_repository_working_directory(
    working_directory: Path, repository_root: Path
) -> None:
    """The reviewer must never run inside the tree it is reviewing.

    It can create files, so a working directory inside the repository is a
    write vector into the very worktree under review.
    """

    if _within(working_directory.resolve(), repository_root.resolve()):
        raise CommandRefused(
            "the Reviewer-1 working directory must be outside the repository under review"
        )


def refuse_unsafe_flag_combination(
    command: Sequence[str], *, repository_root: Path | None = None
) -> None:
    if _has_flag(command, "--mode", "plan") and _has_flag(
        command, "--disable-slash-commands"
    ):
        raise CommandRefused(
            "--disable-slash-commands silently voids --mode plan; refusing unsafe reviewer command"
        )
    # `--add-dir` is the observed write vector: with a repository directory
    # added, the reviewer created a file there; without it, it never escaped its
    # own working directory. Handing it any repository path is therefore giving
    # a reviewer write access to the tree it is reviewing.
    for index, argument in enumerate(command):
        if argument != "--add-dir" or index + 1 >= len(command):
            continue
        if repository_root is None:
            raise CommandRefused(
                "the repository root is required before granting Reviewer-1 an added directory"
            )
        try:
            candidate = Path(command[index + 1]).resolve()
        except (OSError, ValueError):
            raise CommandRefused("--add-dir names an unresolvable path") from None
        if _within(candidate, repository_root.resolve()):
            raise CommandRefused(
                "--add-dir must never grant Reviewer-1 access to the repository under review"
            )


def build_command(
    prompt: str,
    schema: Mapping[str, Any],
    config: Mapping[str, Any],
    *,
    add_dirs: Sequence[str | os.PathLike[str]] = (),
    extra_flags: Sequence[str] = (),
    repository_root: Path | None = None,
) -> list[str]:
    return AGY_ADAPTER.build_command(
        prompt,
        schema,
        config,
        add_dirs=add_dirs,
        extra_flags=extra_flags,
        repository_root=repository_root,
    )


def _valid_finding(value: Any) -> bool:
    return (
        isinstance(value, Mapping)
        and set(value) == {"code", "message"}
        and isinstance(value.get("code"), str)
        and 1 <= len(value["code"]) <= 120
        and isinstance(value.get("message"), str)
        and 1 <= len(value["message"]) <= 2000
    )


def _validate_response(value: Any) -> dict[str, Any]:
    if not isinstance(value, Mapping) or set(value) != {"result", "validation", "findings"}:
        raise ValueError("review response fields do not match the schema")
    result = value.get("result")
    validation = value.get("validation")
    findings = value.get("findings")
    if result not in _ALL_RESULTS:
        raise ValueError("review result is outside the allowed union")
    if validation not in {"PASS", "FAIL", "UNKNOWN"}:
        raise ValueError("validation is outside the allowed enum")
    if not isinstance(findings, list) or len(findings) > 20 or not all(
        _valid_finding(item) for item in findings
    ):
        raise ValueError("findings violate the result schema")
    if result == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE and findings:
        raise ValueError("NO_MATERIAL finding verdict cannot include material findings")
    return {
        "result": result,
        "validation": validation,
        "findings": [dict(item) for item in findings],
    }


_JSON_FENCE = re.compile(r"\A\s*```json\s*\r?\n(.*?)\r?\n```\s*\Z", re.DOTALL)


def _parse_bare_or_fenced_response(text: str) -> dict[str, Any]:
    match = _JSON_FENCE.fullmatch(text)
    payload = match.group(1) if match is not None else text
    return _validate_response(json.loads(payload))


def _parse_event_stream(
    stdout: str, provider: str
) -> tuple[str, dict[str, Any], float | None, dict[str, int | float], str]:
    events = []
    for line in stdout.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, Mapping):
            raise ValueError(f"{provider} event was not an object")
        events.append(event)
    if not events:
        raise ValueError(f"{provider} event stream was empty")

    text_parts: list[str] = []
    conversation_id = "UNAVAILABLE"
    served_model = "UNAVAILABLE"
    completed = False
    for event in events:
        session = event.get("sessionID", event.get("thread_id"))
        if isinstance(session, str) and session:
            conversation_id = session
        model = event.get("model")
        model_id = event.get("modelID")
        model_provider = event.get("providerID")
        if isinstance(model, str) and model:
            served_model = model
        elif isinstance(model_id, str) and model_id:
            served_model = (
                f"{model_provider}/{model_id}"
                if isinstance(model_provider, str) and model_provider
                else model_id
            )
        if provider == "opencode":
            part = event.get("part")
            if event.get("type") == "text" and isinstance(part, Mapping):
                value = part.get("text")
                if isinstance(value, str):
                    text_parts.append(value)
            if (
                event.get("type") == "step_finish"
                and isinstance(part, Mapping)
                and part.get("reason") == "stop"
            ):
                completed = True
        elif (
            event.get("type") == "item.completed"
            and isinstance(event.get("item"), Mapping)
            and event["item"].get("type") == "agent_message"
            and isinstance(event["item"].get("text"), str)
        ):
            text_parts.append(event["item"]["text"])
        if provider == "codex" and event.get("type") == "turn.completed":
            completed = True
        if event.get("type") in {"error", "turn.failed"}:
            raise ValueError(f"{provider} event stream reported a failure")
        part = event.get("part")
        if (
            provider == "opencode"
            and event.get("type") == "tool_use"
            and isinstance(part, Mapping)
            and isinstance(part.get("state"), Mapping)
            and part["state"].get("status") == "error"
        ):
            raise ValueError("OpenCode reported a failed tool")

    if not text_parts:
        raise ValueError(f"{provider} event stream had no final assistant text")
    if not completed:
        raise ValueError(f"{provider} event stream had no successful completion marker")
    final_text = "".join(text_parts) if provider == "opencode" else text_parts[-1]
    response = _parse_bare_or_fenced_response(final_text.strip())
    return conversation_id, response, None, {}, served_model


def _structured_payload(envelope: Mapping[str, Any]) -> Any:
    """Return the schema-enforced result payload.

    With ``--json-schema`` the agy CLI puts the schema-conforming object in
    ``structured_output`` and leaves ``response`` as free prose that merely
    quotes it.  Parsing ``response`` therefore fails on every real call, so the
    structured field is authoritative when present.  A structured value that is
    itself a JSON string is decoded once; anything deeper is left alone and
    fails validation rather than being coaxed into shape.
    """

    structured = envelope.get("structured_output")
    if structured is not None:
        if isinstance(structured, str):
            return json.loads(structured)
        return structured
    response = envelope.get("response")
    if isinstance(response, str):
        return json.loads(response)
    return response


_USAGE_TOKEN_FIELDS = frozenset(
    {
        "input_tokens",
        "output_tokens",
        "total_tokens",
        "prompt_tokens",
        "completion_tokens",
        "cached_tokens",
        "cache_read_tokens",
        "cache_write_tokens",
        "thinking_tokens",
    }
)


def _parse_agy_envelope(
    stdout: str,
    *,
    include_metadata: bool = False,
) -> tuple[str, dict[str, Any]] | tuple[str, dict[str, Any], float, dict[str, int | float]]:
    envelope = json.loads(stdout)
    # The envelope gains fields with the flags in use -- `structured_output`
    # appears only under --json-schema -- so require the known fields to be
    # PRESENT rather than to be the exact set.
    if not isinstance(envelope, Mapping) or not _ENVELOPE_FIELDS.issubset(envelope):
        raise ValueError("agy envelope has an unexpected shape")
    conversation_id = envelope.get("conversation_id")
    status = envelope.get("status")
    duration = envelope.get("duration_seconds")
    turns = envelope.get("num_turns")
    if not isinstance(conversation_id, str) or not conversation_id:
        raise ValueError("agy envelope has no conversation id")
    # The CLI reports "SUCCESS"; comparing case-sensitively rejected every real
    # response and made the reviewer permanently unavailable.
    if not isinstance(status, str) or status.casefold() != "success":
        raise ValueError("agy envelope did not report success")
    if (
        not isinstance(duration, (int, float))
        or isinstance(duration, bool)
        or not math.isfinite(duration)
        or duration < 0
        or not isinstance(turns, int)
        or isinstance(turns, bool)
        or turns < 1
        or not isinstance(envelope.get("usage"), Mapping)
    ):
        raise ValueError("agy envelope metadata has an unexpected shape")
    response = _validate_response(_structured_payload(envelope))
    if not include_metadata:
        return conversation_id, response
    usage = {
        key: value
        for key, value in envelope["usage"].items()
        if key in _USAGE_TOKEN_FIELDS
        and isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value)
        and value >= 0
    }
    return conversation_id, response, duration, usage


def parse_envelope(
    stdout: str,
    *,
    include_metadata: bool = False,
) -> tuple[str, dict[str, Any]] | tuple[str, dict[str, Any], float, dict[str, int | float]]:
    return AGY_ADAPTER.parse(stdout, include_metadata=include_metadata)


def _review_prompt(packet_path: Path) -> str:
    return (
        "You are Reviewer-1. Read and review only the JSON packet at the exact path below. "
        "Do not inspect unrelated files, "
        "infer hidden reasoning, or accept an implementer narrative. Return only the supplied JSON "
        "schema. Use NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE only when the complete diff and supplied "
        "contract establish no material issue. Use UNCERTAIN when the diff or context needed to "
        "assess the diff is insufficient. The normal absence of orchestrator validation evidence "
        "is not by itself grounds for UNCERTAIN; report validation UNKNOWN without inferring that "
        "validation ran.\n\n"
        f"REVIEW_PACKET_PATH={packet_path}"
    )


def invoke_packet(
    packet: PacketBuild,
    config: Mapping[str, Any],
    *,
    runner: Callable[..., subprocess.CompletedProcess[str]] = subprocess.run,
    cwd: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    reviewer_agent: str = "agy",
    reviewer_model: str | None = None,
) -> ReviewOutcome:
    if reviewer_model is not None:
        config = {**config, "reviewer1_model_requested": reviewer_model}
    adapter = REVIEWER_ADAPTERS.get(reviewer_agent)
    if adapter is None:
        return UnavailableReview(
            "REVIEWER_ADAPTER_UNAVAILABLE",
            f"No reviewer adapter is registered for {reviewer_agent}.",
            _requested_model(config),
            provider=reviewer_agent,
        )
    if isinstance(packet, PacketOmitted):
        return _incomplete_packet(packet, config, provider=adapter.provider)
    packet_path: Path | None = None
    scratch_root: Path | None = None
    retried = False
    try:
        timeout = config["reviewer1_timeout_seconds"]
        if (
            not isinstance(timeout, (int, float))
            or isinstance(timeout, bool)
            or not math.isfinite(timeout)
            or timeout <= 0
        ):
            raise ValueError("reviewer1_timeout_seconds must be positive")
        # Containment, not a flag promise.  A live check established that the
        # reviewer CAN create files under `--mode plan --sandbox`; those flags
        # are an agent mode, not a read-only sandbox.  What did hold is that
        # without `--add-dir` it never reached outside its own working
        # directory.  So the reviewer runs in a throwaway directory OUTSIDE the
        # repository, which is removed afterwards, and the repository is never
        # handed to it.  Never run it in the package root -- that is inside the
        # working tree it is reviewing.
        if adapter.provider == OPENCODE_ADAPTER.provider:
            scratch_parent = (
                Path(cwd).resolve() if cwd is not None else Path(tempfile.gettempdir())
            )
            scratch_root = scratch_parent / f"reviewer1-opencode-{uuid.uuid4().hex}"
            scratch_root.mkdir()
            scratch_root = scratch_root.resolve()
            working_directory = scratch_root
        elif cwd is not None:
            working_directory = Path(cwd).resolve()
        else:
            scratch_root = Path(tempfile.mkdtemp(prefix="reviewer1-scratch-")).resolve()
            working_directory = scratch_root
        if repo_root is None:
            raise ValueError("the repository root is required for reviewer confinement")
        repository = Path(repo_root).resolve()
        _refuse_repository_working_directory(working_directory, repository)
        if adapter.provider == OPENCODE_ADAPTER.provider:
            if any(working_directory.iterdir()):
                raise ValueError("OpenCode reviewer working directory must be empty")
            packet_path = working_directory / "review-packet.json"
            handle_context = packet_path.open("xb")
        else:
            handle_context = tempfile.NamedTemporaryFile(
                mode="wb",
                prefix=".reviewer1-packet-",
                suffix=".json",
                dir=working_directory,
                delete=False,
            )
        with handle_context as handle:
            if packet_path is None:
                packet_path = Path(handle.name).resolve()
            handle.write(packet.encoded)
            handle.flush()
            os.fsync(handle.fileno())
        adapter.validate_model(_requested_model(config))
        if adapter.provider == AGY_ADAPTER.provider:
            command = adapter.build_command(
                _review_prompt(packet_path),
                REVIEW_SCHEMA,
                config,
                repository_root=repository,
            )
        else:
            command = adapter.build_command(
                _review_prompt(packet_path), REVIEW_SCHEMA, config, cwd=working_directory
            )
        elapsed_total = 0.0
        attempt_timeout = timeout
        while True:
            started = time.monotonic()
            try:
                completed = adapter.invoke(
                    command,
                    runner=runner,
                    cwd=working_directory,
                    timeout=attempt_timeout,
                )
            except subprocess.TimeoutExpired as error:
                elapsed_total += max(0.0, time.monotonic() - started)
                suffix = " (after 1 retry)" if retried else ""
                return UnavailableReview(
                    "REVIEWER_TIMEOUT",
                    f"Reviewer-1 exceeded its bounded timeout.{suffix}",
                    _requested_model(config),
                    elapsed_seconds=elapsed_total,
                    stderr_tail=_stderr_tail(error.stderr),
                    provider=reviewer_agent,
                )
            elapsed = max(0.0, time.monotonic() - started)
            elapsed_total += elapsed
            returncode = getattr(completed, "returncode", None)
            stdout = getattr(completed, "stdout", None)
            stderr_tail = _stderr_tail(getattr(completed, "stderr", None))
            if not isinstance(returncode, int) or isinstance(returncode, bool) or not isinstance(
                stdout, str
            ):
                raise ValueError("reviewer process returned an unexpected shape")
            if returncode == 0:
                break
            remaining = timeout - elapsed_total
            if (
                not retried
                and elapsed < _FAST_PROCESS_FAILURE_RETRY_SECONDS
                and remaining > _MIN_RETRY_REMAINING_SECONDS
            ):
                retried = True
                attempt_timeout = remaining
                continue
            suffix = " (after 1 retry)" if retried else ""
            return UnavailableReview(
                "REVIEWER_PROCESS_FAILED",
                f"Reviewer-1 exited non-zero ({returncode}).{suffix}",
                _requested_model(config),
                elapsed_seconds=elapsed_total,
                stderr_tail=stderr_tail,
                provider=reviewer_agent,
            )
        try:
            parsed = adapter.parse(stdout, include_metadata=True)
            if adapter.provider == AGY_ADAPTER.provider:
                conversation_id, response, duration_seconds, usage = parsed
                served_model = "UNAVAILABLE"
            else:
                conversation_id, response, duration_seconds, usage, served_model = parsed
        except (json.JSONDecodeError, TypeError, ValueError) as error:
            return UnavailableReview(
                "REVIEWER_RESPONSE_INVALID",
                f"Reviewer-1 response or configuration was invalid ({type(error).__name__})."
                + (" (after 1 retry)" if retried else ""),
                _requested_model(config),
                raw_stdout=stdout[-262144:],
                decoder_detail=str(error)[:200],
                elapsed_seconds=elapsed_total,
                provider=reviewer_agent,
            )
        if response["result"] == policy.REVIEWER1_UNAVAILABLE:
            return UnavailableReview(
                "REVIEWER_MODEL_REPORTED_UNAVAILABLE",
                "Reviewer-1 returned REVIEWER1_UNAVAILABLE."
                + (" (after 1 retry)" if retried else ""),
                _requested_model(config),
                duration_seconds,
                tuple(sorted(usage.items())),
                elapsed_seconds=elapsed_total,
                provider=reviewer_agent,
            )
        result = response["result"]
        findings = response["findings"]
        if packet.added_redaction_count:
            files = ", ".join(packet.added_affected_files)
            redaction_label = (
                "redaction" if packet.added_redaction_count == 1 else "redactions"
            )
            findings = [
                *findings,
                _finding(
                    "REVIEW_PACKET_ADDED_SECRET_REDACTED",
                    "Added credential-like text was redacted from "
                    f"{files} ({packet.added_redaction_count} added-line "
                    f"{redaction_label}); forcing FULL Claude review.",
                ),
            ]
            if result == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE:
                result = policy.UNCERTAIN
        return _completed(
            result,
            response["validation"],
            findings,
            conversation_id,
            config,
            duration_seconds,
            usage,
            provider=adapter.provider,
            served_model_attestation=served_model,
        )
    except FileNotFoundError:
        return UnavailableReview(
            "REVIEWER_EXECUTABLE_MISSING",
            "The configured Reviewer-1 executable was not found."
            + (" (after 1 retry)" if retried else ""),
            _requested_model(config),
            provider=reviewer_agent,
        )
    except (json.JSONDecodeError, TypeError, ValueError, OSError) as error:
        return UnavailableReview(
            "REVIEWER_RESPONSE_INVALID",
            f"Reviewer-1 response or configuration was invalid ({type(error).__name__})."
            + (" (after 1 retry)" if retried else ""),
            _requested_model(config),
            provider=reviewer_agent,
        )
    except Exception as error:
        return UnavailableReview(
            "REVIEWER_UNEXPECTED_FAILURE",
            f"Reviewer-1 failed unexpectedly ({type(error).__name__})."
            + (" (after 1 retry)" if retried else ""),
            _requested_model(config),
            provider=adapter.provider,
        )
    finally:
        if packet_path is not None:
            try:
                packet_path.unlink()
            except OSError:
                pass
        if scratch_root is not None:
            shutil.rmtree(scratch_root, ignore_errors=True)


def review_materials(
    materials: Mapping[str, Any],
    *,
    config: Mapping[str, Any] | None = None,
    runner: Callable[..., subprocess.CompletedProcess[str]] = subprocess.run,
    cwd: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    reviewer_agent: str = "agy",
    reviewer_model: str | None = None,
    reviewer_timeout_seconds: float | None = None,
) -> ReviewOutcome:
    try:
        loaded = _load_config(config)
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        fallback = config if isinstance(config, Mapping) else {}
        return _unavailable(
            "REVIEWER_CONFIG_INVALID",
            f"Reviewer-1 configuration was invalid ({type(error).__name__}).",
            fallback,
            provider=reviewer_agent,
        )
    if reviewer_model is not None or reviewer_timeout_seconds is not None:
        loaded = dict(loaded)
        if reviewer_model is not None:
            loaded["reviewer1_model_requested"] = reviewer_model
        if reviewer_timeout_seconds is not None:
            loaded["reviewer1_timeout_seconds"] = reviewer_timeout_seconds
    if isinstance(loaded, dict):
        loaded["reviewer1_agent"] = reviewer_agent
    try:
        packet = build_review_packet(materials, loaded["reviewer1_packet_max_bytes"])
    except Exception as error:
        return _unavailable(
            "REVIEW_PACKET_BUILD_FAILED",
            f"The review packet failed unexpectedly ({type(error).__name__}).",
            loaded,
            provider=reviewer_agent,
        )
    if isinstance(packet, PacketOmitted):
        adapter = REVIEWER_ADAPTERS.get(reviewer_agent)
        return _incomplete_packet(
            packet,
            loaded,
            provider=adapter.provider if adapter is not None else reviewer_agent,
        )
    if cwd is not None:
        return invoke_packet(
            packet, loaded, runner=runner, cwd=cwd, repo_root=repo_root,
            reviewer_agent=reviewer_agent, reviewer_model=reviewer_model,
        )
    try:
        # Deliberately the system temp root, NOT the package directory: a live
        # check showed the reviewer can create files even under
        # `--mode plan --sandbox`, so its working directory must sit outside the
        # worktree it is reviewing.
        with tempfile.TemporaryDirectory(prefix="reviewer1-") as scratch:
            return invoke_packet(
                packet, loaded, runner=runner, cwd=scratch, repo_root=repo_root,
                reviewer_agent=reviewer_agent, reviewer_model=reviewer_model,
            )
    except OSError as error:
        return _unavailable(
            "REVIEWER_WORKDIR_UNAVAILABLE",
            f"The isolated Reviewer-1 working directory failed ({type(error).__name__}).",
            loaded,
            provider=reviewer_agent,
        )


def _load_config(config: Mapping[str, Any] | None) -> Mapping[str, Any]:
    loaded = (
        json.loads(_CONFIG_PATH.read_text(encoding="utf-8")) if config is None else config
    )
    timeout = loaded.get("reviewer1_timeout_seconds")
    budget = loaded.get("reviewer1_packet_max_bytes")
    model = loaded.get("reviewer1_model_requested")
    if not isinstance(model, str) or not model:
        raise ValueError("reviewer1_model_requested must be a non-empty string")
    if loaded.get("reviewer1_agent", AGY_ADAPTER.provider) == AGY_ADAPTER.provider:
        AGY_ADAPTER.validate_model(model)
    if (
        not isinstance(timeout, (int, float))
        or isinstance(timeout, bool)
        or not math.isfinite(timeout)
        or timeout <= 0
    ):
        raise ValueError("reviewer1_timeout_seconds must be positive")
    if not isinstance(budget, int) or isinstance(budget, bool) or budget <= 0:
        raise ValueError("reviewer1_packet_max_bytes must be a positive integer")
    return loaded


def _markdown_sections(path: Path, headings: Sequence[str]) -> str:
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return ""
    wanted = {heading.casefold() for heading in headings}
    lines = text.splitlines()
    selected: list[str] = []
    index = 0
    while index < len(lines):
        match = re.match(r"^(#{1,6})\s+(.+?)\s*$", lines[index])
        if match is None or match.group(2).casefold() not in wanted:
            index += 1
            continue
        level = len(match.group(1))
        selected.append(lines[index])
        index += 1
        while index < len(lines):
            next_heading = re.match(r"^(#{1,6})\s+", lines[index])
            if next_heading is not None and len(next_heading.group(1)) <= level:
                break
            selected.append(lines[index])
            index += 1
    return "\n".join(selected).strip()


_DEFAULT_AUTHORITY_SOURCES = (
    {
        "path": "AGENTS.md",
        "headings": _AUTHORITY_HEADINGS,
    },
)


def _authority_slice(
    repo_root: Path,
    changed_paths: Sequence[str],
    sources: Sequence[Any] | None = None,
) -> str:
    slices: list[str] = []
    root = repo_root.resolve()
    for source in _DEFAULT_AUTHORITY_SOURCES if sources is None else sources:
        if isinstance(source, Mapping):
            path = source.get("path")
            headings = source.get("headings")
            when_changed = source.get("when_changed")
        else:
            path = getattr(source, "path", None)
            headings = getattr(source, "headings", None)
            when_changed = getattr(source, "when_changed", None)
        if (
            not isinstance(path, str)
            or not isinstance(headings, Sequence)
            or isinstance(headings, (str, bytes))
            or not all(isinstance(heading, str) and heading for heading in headings)
            or (when_changed is not None and not isinstance(when_changed, str))
        ):
            continue
        normalized_path = path.replace("\\", "/")
        while normalized_path.startswith("./"):
            normalized_path = normalized_path[2:]
        if (
            not normalized_path
            or normalized_path.startswith("/")
            or ".." in normalized_path.split("/")
            or Path(path).is_absolute()
            or PureWindowsPath(path).is_absolute()
            or PureWindowsPath(path).drive
        ):
            continue
        normalized_changed = [changed.replace("\\", "/") for changed in changed_paths]
        if when_changed is not None and not any(
            changed.startswith(when_changed.replace("\\", "/"))
            for changed in normalized_changed
        ):
            continue
        try:
            resolved_path = (repo_root / normalized_path).resolve()
        except (OSError, RuntimeError, ValueError):
            continue
        if not _within(resolved_path, root):
            continue
        slices.append(_markdown_sections(resolved_path, headings))
    result = "\n\n".join(item for item in slices if item)
    return result or "No readable repository authority slice was available; treat authority as UNKNOWN."


def _safe_delta_paths(delta: Mapping[str, Any]) -> list[str]:
    raw_paths = delta.get("changed_paths")
    if not isinstance(raw_paths, list) or not raw_paths:
        raise ValueError("delta has no changed paths")
    paths: list[str] = []
    for raw in raw_paths:
        if not isinstance(raw, str) or not raw:
            raise ValueError("delta contains an invalid path")
        normalized = raw.replace("\\", "/")
        path = Path(normalized)
        if path.is_absolute() or ".." in path.parts or normalized in {".", ""}:
            raise ValueError("delta contains an unsafe path")
        paths.append(normalized)
    return sorted(set(paths))


def _run_git(repo_root: Path, args: Sequence[str]) -> str:
    completed = subprocess.run(
        ["git", *args],
        cwd=repo_root,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="strict",
        timeout=30,
        check=False,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
    )
    if completed.returncode != 0:
        raise ValueError("git could not reconstruct the delegated diff")
    return completed.stdout


def _new_file_diff(repo_root: Path, path: str) -> str:
    current = (repo_root / path).resolve()
    if not _within(current, repo_root.resolve()) or not current.is_file():
        raise ValueError("the complete new-file content is unavailable")
    completed = subprocess.run(
        [
            "git",
            "diff",
            "--binary",
            "--no-index",
            "--no-renames",
            "--no-ext-diff",
            "--no-textconv",
            "--",
            os.devnull,
            path,
        ],
        cwd=repo_root,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="strict",
        timeout=30,
        check=False,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
    )
    if completed.returncode not in {0, 1}:
        raise ValueError("git could not reconstruct the complete new-file addition")
    if completed.stdout:
        return completed.stdout
    if completed.returncode == 0 and current.stat().st_size == 0:
        return f"NEW FILE {json.dumps(path)}: complete current content is empty (0 bytes).\n"
    raise ValueError("git produced no complete new-file addition")


def _source_paths(delta: Mapping[str, Any], source: str) -> set[str]:
    sources = delta.get("sources")
    entry = sources.get(source) if isinstance(sources, Mapping) else None
    raw_paths = entry.get("paths") if isinstance(entry, Mapping) else None
    if not isinstance(raw_paths, list) or not all(
        isinstance(path, str) and path for path in raw_paths
    ):
        raise ValueError(f"delta source {source!r} has invalid paths")
    return {path.replace("\\", "/") for path in raw_paths}


def _review_paths(
    delta: Mapping[str, Any], classification: Mapping[str, Any] | None
) -> list[str]:
    changed = set(_safe_delta_paths(delta))
    if classification is None:
        return sorted(changed)
    raw_paths = classification.get("in_scope_paths")
    if not isinstance(raw_paths, list) or not raw_paths:
        raise ValueError("concurrent-writer classification has no delegated paths")
    selected = _safe_delta_paths({"changed_paths": raw_paths})
    if not set(selected).issubset(changed):
        raise ValueError("concurrent-writer classification escaped the known delta")
    return selected


def _delegated_diff(
    repo_root: Path,
    delta: Mapping[str, Any],
    classification: Mapping[str, Any] | None = None,
) -> tuple[list[str], str]:
    paths = _review_paths(delta, classification)
    baseline = delta.get("baseline_head")
    if not isinstance(baseline, str) or not baseline:
        raise ValueError("delta has no baseline head")
    ambiguous = delta.get("ambiguous_paths")
    if not isinstance(ambiguous, list) or not all(isinstance(path, str) for path in ambiguous):
        raise ValueError("delta has invalid ambiguity evidence")
    overlap = sorted(set(paths).intersection(path.replace("\\", "/") for path in ambiguous))
    if overlap:
        raise ValueError(
            "the delegated diff cannot be isolated from pre-existing same-path changes: "
            + ", ".join(overlap)
        )
    manifest_paths = set(paths).intersection(_source_paths(delta, "scoped_manifest"))
    pathspecs = [f":(literal){path}" for path in paths]
    baseline_names = {
        path.replace("\\", "/")
        for path in _run_git(
            repo_root,
            ["ls-tree", "-r", "--name-only", baseline, "--", *pathspecs],
        ).splitlines()
        if path
    }
    unresolved = sorted(manifest_paths.intersection(baseline_names))
    if unresolved:
        raise ValueError(
            "the accepted seam has hashes but no baseline contents for changed untracked or "
            "ignored paths: " + ", ".join(unresolved)
        )
    new_at_baseline = manifest_paths.difference(baseline_names)
    names = {
        path.replace("\\", "/")
        for path in _run_git(
            repo_root,
            [
                "diff",
                "--name-only",
                "--no-renames",
                "--no-ext-diff",
                "--no-textconv",
                baseline,
                "--",
                *pathspecs,
            ],
        ).splitlines()
        if path
    }
    omitted = set(paths).difference(names)
    unavailable = sorted(omitted.difference(new_at_baseline))
    if unavailable or not names.issubset(paths):
        raise ValueError(
            "the complete delegated diff is unavailable for: "
            + ", ".join(unavailable or sorted(names.difference(paths)))
        )
    tracked_diff = _run_git(
        repo_root,
        [
            "diff",
            "--binary",
            "--no-renames",
            "--no-ext-diff",
            "--no-textconv",
            baseline,
            "--",
            *pathspecs,
        ],
    )
    additions = [_new_file_diff(repo_root, path) for path in sorted(omitted)]
    diff = tracked_diff + "".join(additions)
    if not diff:
        raise ValueError("the computed non-empty delta produced no reviewable diff")
    return paths, diff


def _seam_materials(
    task_id: str,
    repo_root: Path,
    delta: Mapping[str, Any],
    classification: Mapping[str, Any] | None,
    validation_summary: str | None = None,
    task_objective: str | None = None,
    acceptance_criteria: str | None = None,
    authority_sources: Sequence[Any] | None = None,
) -> Mapping[str, str]:
    paths, diff = _delegated_diff(repo_root, delta, classification)
    decision = classification.get("decision") if isinstance(classification, Mapping) else "UNKNOWN"
    if validation_summary is None:
        validation_evidence = (
            "No orchestrator validation evidence was supplied to this review. This is the normal "
            "review-seam default and is not, by itself, grounds for UNCERTAIN."
        )
    elif not isinstance(validation_summary, str) or not validation_summary.strip():
        raise ValueError("validation evidence must be non-empty text when supplied")
    elif validation_summary.strip().startswith(("Worker-reported", "Orchestrator-")):
        validation_evidence = validation_summary.strip()
    else:
        validation_evidence = (
            "Orchestrator-supplied validation evidence: " + validation_summary.strip()
        )
    base_objective = (
        f"Review the complete delegated change for task {task_id!r}, bounded to these actual "
        f"changed paths: {', '.join(paths)}."
    )
    if task_objective and task_objective.strip():
        objective_content = f"{base_objective} Task Objective: {task_objective.strip()}"
    else:
        objective_content = base_objective

    base_criteria = (
        "Accept only if the complete delegated diff is correct within the named bounded task, "
        "preserves current contracts, and introduces no material regression or scope expansion. "
        "Consider supplied validation evidence, but do not treat its normal absence as evidence "
        "that the diff is incomplete."
    )
    if acceptance_criteria and acceptance_criteria.strip():
        criteria_content = f"{base_criteria} Specific Task Criteria: {acceptance_criteria.strip()}"
    else:
        criteria_content = base_criteria

    return {
        "objective": objective_content,
        "acceptance_criteria": criteria_content,
        "authority_contract": _authority_slice(repo_root, paths, authority_sources),
        "delegated_diff": diff,
        "validation_summary": (
            f"{validation_evidence} "
            f"Ledger delta: {delta.get('changed_file_count')} files, "
            f"{delta.get('changed_line_count')} changed lines; concurrent-writer decision: {decision}. "
            "Raw agent output and full success logs were not forwarded."
        ),
    }


def review(
    *,
    task_id: str,
    phase: str,
    route: str,
    model: str,
    effort: str,
    repo_root: str | os.PathLike[str],
    delta: Mapping[str, Any],
    classification: Mapping[str, Any] | None,
    validation_summary: str | None = None,
    task_objective: str | None = None,
    acceptance_criteria: str | None = None,
    reviewer_executable: str | None = None,
    authority_sources: Sequence[Any] | None = None,
    reviewer_agent: str = "agy",
    reviewer_model: str | None = None,
    reviewer_timeout_seconds: float | None = None,
) -> ReviewOutcome:
    """Reviewer callable matching the accepted ``codex.py`` seam."""

    del phase, route, model, effort
    try:
        config = _load_config(None)
        if reviewer_executable is not None:
            reviewer_config = dict(config)
            reviewer_config["reviewer1_executable"] = reviewer_executable
            config = _load_config(reviewer_config)
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        return _unavailable(
            "REVIEWER_CONFIG_INVALID",
            f"Reviewer-1 configuration was invalid ({type(error).__name__}).",
            {"reviewer1_model_requested": PINNED_MODEL},
        )
    try:
        materials = _seam_materials(
            task_id,
            Path(repo_root).resolve(),
            delta,
            classification,
            validation_summary,
            task_objective=task_objective,
            acceptance_criteria=acceptance_criteria,
            authority_sources=authority_sources,
        )
        return review_materials(
            materials, config=config, repo_root=repo_root,
            reviewer_agent=reviewer_agent, reviewer_model=reviewer_model,
            reviewer_timeout_seconds=reviewer_timeout_seconds,
        )
    except (OSError, UnicodeDecodeError, ValueError) as error:
        error_text = str(error)[:200]
        packet = PacketOmitted(
            "REVIEW_PACKET_INCOMPLETE",
            f"The complete delegated review packet could not be built ({type(error).__name__}: {error_text}); no model call was made.",
            ("delegated_diff",),
        )
        return _incomplete_packet(packet, config)
    except Exception as error:
        return _unavailable(
            "REVIEWER_UNEXPECTED_FAILURE",
            f"Reviewer-1 failed unexpectedly ({type(error).__name__}).",
            config,
        )


review.available = True  # type: ignore[attr-defined]


__all__ = [
    "CommandRefused",
    "CompletedReview",
    "IncompleteReview",
    "PacketOmitted",
    "PacketReady",
    "PINNED_MODEL",
    "REVIEW_SCHEMA",
    "REVIEWER_ADAPTERS",
    "ReviewOutcome",
    "UnavailableReview",
    "build_command",
    "build_review_packet",
    "invoke_packet",
    "parse_envelope",
    "refuse_unsafe_flag_combination",
    "review",
    "review_materials",
    "register_reviewer_adapter",
]
