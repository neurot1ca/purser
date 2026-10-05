"""JSON subprocess boundary for the Python Reviewer-1 seam."""

from __future__ import annotations

import json
import math
import sys
from typing import Any

import policy
import reviewer


_REQUIRED = {"task_id", "phase", "route", "model", "effort", "repo_root", "delta", "classification"}
_OPTIONAL_TYPES = {
    "thread_id": (str, type(None)),
    "project_key": (str, type(None)),
    "state_root": (str, type(None)),
    "validation_summary": (str, type(None)),
    "task_objective": (str, type(None)),
    "acceptance_criteria": (str, type(None)),
    "reviewer_executable": (str, type(None)),
    "authority_sources": (list, type(None)),
    "reviewer_agent": (str,),
    "reviewer_model": (str, type(None)),
    "reviewer_timeout_seconds": (int, float, type(None)),
}
_STRING_TYPES = (str,)


class _InputError(ValueError):
    pass


def _reject_constant(value: str) -> None:
    raise _InputError(f"invalid JSON constant: {value}")


def _arguments(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise _InputError("input must be a JSON object")
    allowed = _REQUIRED | set(_OPTIONAL_TYPES)
    unknown = value.keys() - allowed
    if unknown:
        raise _InputError(f"unknown key: {sorted(unknown)[0]}")
    missing = _REQUIRED - value.keys()
    if missing:
        raise _InputError(f"missing key: {sorted(missing)[0]}")
    for key in ("task_id", "phase", "route", "model", "effort", "repo_root"):
        if not isinstance(value[key], _STRING_TYPES):
            raise _InputError(f"{key} must be a string")
    if not isinstance(value["delta"], dict):
        raise _InputError("delta must be an object")
    if value["classification"] is not None and not isinstance(value["classification"], dict):
        raise _InputError("classification must be an object or null")
    for key, types in _OPTIONAL_TYPES.items():
        if key in value and (not isinstance(value[key], types) or
                             (key == "reviewer_timeout_seconds" and isinstance(value[key], bool))):
            raise _InputError(f"{key} has the wrong type")
    timeout = value.get("reviewer_timeout_seconds")
    if isinstance(timeout, (int, float)) and not math.isfinite(timeout):
        raise _InputError("reviewer_timeout_seconds must be finite")
    return value


def _outcome(value: Any) -> dict[str, Any]:
    if isinstance(value, reviewer.CompletedReview):
        kind = "completed"
    elif isinstance(value, reviewer.UnavailableReview):
        kind = "unavailable"
    elif isinstance(value, reviewer.IncompleteReview):
        kind = "incomplete"
    else:
        raise TypeError("review returned an unknown outcome")
    return {"outcome": kind, **dict(value)}


def main() -> int:
    try:
        raw = sys.stdin.read()
        try:
            payload = json.loads(raw, parse_constant=_reject_constant)
        except (json.JSONDecodeError, UnicodeDecodeError) as error:
            raise _InputError("stdin must contain valid JSON") from error
        kwargs = _arguments(payload)
    except _InputError as error:
        json.dump({"error": "INVALID_INPUT", "message": str(error)}, sys.stdout)
        sys.stdout.write("\n")
        return 2

    try:
        output = _outcome(reviewer.review(**kwargs))
    except Exception as error:
        model = kwargs.get("reviewer_model") or "UNAVAILABLE"
        output = {
            "outcome": "unavailable",
            "reason": "REVIEWER_BRIDGE_EXCEPTION",
            "review_status": "UNAVAILABLE",
            "available": False,
            "provider": kwargs.get("reviewer_agent", "agy"),
            "result": policy.REVIEWER1_UNAVAILABLE,
            "validation": "UNKNOWN",
            "findings": [{
                "code": "REVIEWER_BRIDGE_EXCEPTION",
                "message": f"Reviewer-1 failed unexpectedly ({type(error).__name__}).",
            }],
            "error_code": "REVIEWER_BRIDGE_EXCEPTION",
            "reviewer1_model_requested": model,
            "served_model_attestation": "UNAVAILABLE",
        }
    json.dump(output, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
