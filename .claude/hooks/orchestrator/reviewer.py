"""Bounded Antigravity Reviewer-1 transport."""

from __future__ import annotations

from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
from typing import Any

import policy


_PACKAGE_ROOT = Path(__file__).resolve().parent
_CONFIG_PATH = _PACKAGE_ROOT / "config.json"
PINNED_MODEL = "gemini-3.7-flash-high"
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

    def __post_init__(self) -> None:
        if self.result not in _COMPLETED_RESULTS:
            raise ValueError("a completed review cannot carry a failure result")

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "COMPLETED",
            "available": True,
            "provider": "agy",
            "result": self.result,
            "validation": self.validation,
            "findings": [
                {"code": code, "message": message} for code, message in self.findings
            ],
            "conversation_id": self.conversation_id,
            "reviewer1_model_requested": self.reviewer1_model_requested,
            "served_model_attestation": "UNAVAILABLE",
        }


@dataclass(frozen=True)
class UnavailableReview(_ResultMapping):
    """A failed invocation; it cannot carry a passing reviewer result."""

    error_code: str
    message: str
    reviewer1_model_requested: str

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "UNAVAILABLE",
            "available": False,
            "provider": "agy",
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

    def _values(self) -> dict[str, Any]:
        return {
            "review_status": "NOT_RUN_INCOMPLETE_PACKET",
            "available": True,
            "provider": "agy",
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
) -> CompletedReview:
    return CompletedReview(
        result=result,
        validation=validation,
        findings=tuple((item["code"], item["message"]) for item in findings),
        conversation_id=conversation_id,
        reviewer1_model_requested=_requested_model(config),
    )


def _unavailable(code: str, message: str, config: Mapping[str, Any]) -> UnavailableReview:
    return UnavailableReview(code, message, _requested_model(config))


def _incomplete_packet(packet: PacketOmitted, config: Mapping[str, Any]) -> IncompleteReview:
    return IncompleteReview(
        packet.code,
        packet.message,
        packet.omitted,
        _requested_model(config),
    )


def _canonical_json(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")


def _contains_forbidden_text(value: str) -> bool:
    return any(pattern.search(value) for pattern in _FORBIDDEN_TEXT)


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
    for field in _PACKET_FIELDS:
        value = materials[field]
        if not isinstance(value, str) or not value.strip():
            return PacketOmitted(
                "REVIEW_PACKET_VALUE_INVALID",
                f"The required packet field {field!r} was empty or non-text; no context was sent.",
                (field,),
            )
        if _contains_forbidden_text(value):
            return PacketOmitted(
                "REVIEW_PACKET_SENSITIVE_CONTENT",
                f"Credential-like or excluded narrative material was detected in {field!r}; it was not sent.",
                (field,),
            )
        packet[field] = value

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
    return PacketReady(packet, encoded)


def _has_flag(command: Sequence[str], name: str, value: str | None = None) -> bool:
    for index, item in enumerate(command):
        if item == name:
            return value is None or (index + 1 < len(command) and command[index + 1] == value)
        if item.startswith(name + "="):
            return value is None or item == f"{name}={value}"
    return False


_REPOSITORY_ROOT = _PACKAGE_ROOT.parents[2]


def _within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


def _refuse_repository_working_directory(working_directory: Path) -> None:
    """The reviewer must never run inside the tree it is reviewing.

    It can create files, so a working directory inside the repository is a
    write vector into the very worktree under review.
    """

    if _within(working_directory.resolve(), _REPOSITORY_ROOT):
        raise CommandRefused(
            "the Reviewer-1 working directory must be outside the repository under review"
        )


def refuse_unsafe_flag_combination(command: Sequence[str]) -> None:
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
        try:
            candidate = Path(command[index + 1]).resolve()
        except (OSError, ValueError):
            raise CommandRefused("--add-dir names an unresolvable path") from None
        if _within(candidate, _REPOSITORY_ROOT):
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
) -> list[str]:
    executable = config.get("reviewer1_executable")
    model = config.get("reviewer1_model_requested")
    if not isinstance(executable, str) or not os.path.isabs(executable):
        raise ValueError("reviewer1_executable must be an absolute path")
    if model != PINNED_MODEL:
        raise ValueError(f"reviewer1_model_requested must equal the pinned id {PINNED_MODEL}")
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
    refuse_unsafe_flag_combination(command)
    return command


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
    return {
        "result": result,
        "validation": validation,
        "findings": [dict(item) for item in findings],
    }


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


def parse_envelope(stdout: str) -> tuple[str, dict[str, Any]]:
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
    return conversation_id, _validate_response(_structured_payload(envelope))


def _review_prompt(packet_path: Path) -> str:
    return (
        "You are Reviewer-1. Read and review only the JSON packet at the exact path below. "
        "Do not inspect unrelated files, "
        "infer hidden reasoning, or accept an implementer narrative. Return only the supplied JSON "
        "schema. Use NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE only when the complete diff and supplied "
        "contract establish no material issue. Use UNCERTAIN for insufficient context or validation.\n\n"
        f"REVIEW_PACKET_PATH={packet_path}"
    )


def invoke_packet(
    packet: PacketBuild,
    config: Mapping[str, Any],
    *,
    runner: Callable[..., subprocess.CompletedProcess[str]] = subprocess.run,
    cwd: str | os.PathLike[str] | None = None,
) -> ReviewOutcome:
    if isinstance(packet, PacketOmitted):
        return _incomplete_packet(packet, config)
    packet_path: Path | None = None
    scratch_root: Path | None = None
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
        if cwd is not None:
            working_directory = Path(cwd).resolve()
        else:
            scratch_root = Path(tempfile.mkdtemp(prefix="reviewer1-scratch-")).resolve()
            working_directory = scratch_root
        _refuse_repository_working_directory(working_directory)
        with tempfile.NamedTemporaryFile(
            mode="wb",
            prefix=".reviewer1-packet-",
            suffix=".json",
            dir=working_directory,
            delete=False,
        ) as handle:
            packet_path = Path(handle.name).resolve()
            handle.write(packet.encoded)
            handle.flush()
            os.fsync(handle.fileno())
        command = build_command(_review_prompt(packet_path), REVIEW_SCHEMA, config)
        completed = runner(
            command,
            cwd=working_directory,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
            check=False,
        )
        returncode = getattr(completed, "returncode", None)
        stdout = getattr(completed, "stdout", None)
        if not isinstance(returncode, int) or isinstance(returncode, bool) or not isinstance(
            stdout, str
        ):
            raise ValueError("reviewer process returned an unexpected shape")
        if returncode != 0:
            return _unavailable(
                "REVIEWER_PROCESS_FAILED",
                f"Reviewer-1 exited non-zero ({returncode}).",
                config,
            )
        conversation_id, response = parse_envelope(stdout)
        if response["result"] == policy.REVIEWER1_UNAVAILABLE:
            return _unavailable(
                "REVIEWER_MODEL_REPORTED_UNAVAILABLE",
                "Reviewer-1 returned REVIEWER1_UNAVAILABLE.",
                config,
            )
        return _completed(
            response["result"],
            response["validation"],
            response["findings"],
            conversation_id,
            config,
        )
    except subprocess.TimeoutExpired:
        return _unavailable("REVIEWER_TIMEOUT", "Reviewer-1 exceeded its bounded timeout.", config)
    except FileNotFoundError:
        return _unavailable(
            "REVIEWER_EXECUTABLE_MISSING", "The configured Reviewer-1 executable was not found.", config
        )
    except (json.JSONDecodeError, TypeError, ValueError, OSError) as error:
        return _unavailable(
            "REVIEWER_RESPONSE_INVALID",
            f"Reviewer-1 response or configuration was invalid ({type(error).__name__}).",
            config,
        )
    except Exception as error:
        return _unavailable(
            "REVIEWER_UNEXPECTED_FAILURE",
            f"Reviewer-1 failed unexpectedly ({type(error).__name__}).",
            config,
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
) -> ReviewOutcome:
    try:
        loaded = _load_config(config)
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        fallback = config if isinstance(config, Mapping) else {}
        return _unavailable(
            "REVIEWER_CONFIG_INVALID",
            f"Reviewer-1 configuration was invalid ({type(error).__name__}).",
            fallback,
        )
    try:
        packet = build_review_packet(materials, loaded["reviewer1_packet_max_bytes"])
    except Exception as error:
        return _unavailable(
            "REVIEW_PACKET_BUILD_FAILED",
            f"The review packet failed unexpectedly ({type(error).__name__}).",
            loaded,
        )
    if isinstance(packet, PacketOmitted):
        return _incomplete_packet(packet, loaded)
    if cwd is not None:
        return invoke_packet(packet, loaded, runner=runner, cwd=cwd)
    try:
        # Deliberately the system temp root, NOT the package directory: a live
        # check showed the reviewer can create files even under
        # `--mode plan --sandbox`, so its working directory must sit outside the
        # worktree it is reviewing.
        with tempfile.TemporaryDirectory(prefix="reviewer1-") as scratch:
            return invoke_packet(packet, loaded, runner=runner, cwd=scratch)
    except OSError as error:
        return _unavailable(
            "REVIEWER_WORKDIR_UNAVAILABLE",
            f"The isolated Reviewer-1 working directory failed ({type(error).__name__}).",
            loaded,
        )


def _load_config(config: Mapping[str, Any] | None) -> Mapping[str, Any]:
    loaded = (
        json.loads(_CONFIG_PATH.read_text(encoding="utf-8")) if config is None else config
    )
    executable = loaded.get("reviewer1_executable")
    timeout = loaded.get("reviewer1_timeout_seconds")
    budget = loaded.get("reviewer1_packet_max_bytes")
    model = loaded.get("reviewer1_model_requested")
    if not isinstance(executable, str) or not os.path.isabs(executable):
        raise ValueError("reviewer1_executable must be an absolute path")
    if model != PINNED_MODEL:
        raise ValueError(f"reviewer1_model_requested must equal the pinned id {PINNED_MODEL}")
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


def _authority_slice(repo_root: Path, changed_paths: Sequence[str]) -> str:
    slices = [_markdown_sections(repo_root / "AGENTS.md", _AUTHORITY_HEADINGS)]
    if any(path.replace("\\", "/").startswith(".claude/hooks/orchestrator/") for path in changed_paths):
        slices.append(_markdown_sections(_PACKAGE_ROOT / "BUILD_SPEC.md", _BUILD_SPEC_HEADINGS))
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
    )
    if completed.returncode != 0:
        raise ValueError("git could not reconstruct the delegated diff")
    return completed.stdout


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
    manifest_paths = _source_paths(delta, "scoped_manifest")
    unresolved = sorted(set(paths).intersection(manifest_paths))
    if unresolved:
        raise ValueError(
            "the accepted seam has hashes but no baseline contents for changed untracked or "
            "ignored paths: " + ", ".join(unresolved)
        )
    pathspecs = [f":(literal){path}" for path in paths]
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
    if names != set(paths):
        omitted = sorted(set(paths).difference(names))
        raise ValueError("the complete delegated diff is unavailable for: " + ", ".join(omitted))
    diff = _run_git(
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
    if not diff:
        raise ValueError("the computed non-empty delta produced no reviewable diff")
    return paths, diff


def _seam_materials(
    task_id: str,
    repo_root: Path,
    delta: Mapping[str, Any],
    classification: Mapping[str, Any] | None,
) -> Mapping[str, str]:
    paths, diff = _delegated_diff(repo_root, delta, classification)
    decision = classification.get("decision") if isinstance(classification, Mapping) else "UNKNOWN"
    return {
        "objective": (
            f"Review the complete delegated change for task {task_id!r}, bounded to these actual "
            f"changed paths: {', '.join(paths)}."
        ),
        "acceptance_criteria": (
            "Accept only if the complete delegated diff is correct within the named bounded task, "
            "preserves current contracts, introduces no material regression or scope expansion, "
            "and the supplied validation evidence is sufficient."
        ),
        "authority_contract": _authority_slice(repo_root, paths),
        "delegated_diff": diff,
        "validation_summary": (
            "Focused validation outcome exposed by the accepted reviewer seam: UNKNOWN. "
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
) -> ReviewOutcome:
    """Reviewer callable matching the accepted ``codex.py`` seam."""

    del phase, route, model, effort
    try:
        config = _load_config(None)
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        return _unavailable(
            "REVIEWER_CONFIG_INVALID",
            f"Reviewer-1 configuration was invalid ({type(error).__name__}).",
            {"reviewer1_model_requested": PINNED_MODEL},
        )
    try:
        materials = _seam_materials(task_id, Path(repo_root).resolve(), delta, classification)
        return review_materials(materials, config=config)
    except (OSError, UnicodeDecodeError, ValueError) as error:
        packet = PacketOmitted(
            "REVIEW_PACKET_INCOMPLETE",
            f"The complete delegated review packet could not be built ({type(error).__name__}); no model call was made.",
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
    "ReviewOutcome",
    "UnavailableReview",
    "build_command",
    "build_review_packet",
    "invoke_packet",
    "parse_envelope",
    "refuse_unsafe_flag_combination",
    "review",
    "review_materials",
]
