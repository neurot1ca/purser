from __future__ import annotations

import json
import sys
from typing import Any

import readiness_check


_REQUIRED = {"grounded", "objective", "acceptance_criteria", "attributed_diff",
             "worker_report_untrusted", "agent", "model", "repo_root"}
_OPTIONAL = {"executable"}
class _InputError(ValueError):
    pass
def _arguments(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise _InputError("input must be a JSON object")
    if value.keys() - _REQUIRED - _OPTIONAL:
        raise _InputError("unknown input key")
    if _REQUIRED - value.keys():
        raise _InputError("missing input key")
    if value["grounded"] is not True:
        raise _InputError("grounded must be true")
    for key in _REQUIRED - {"grounded"}:
        if not isinstance(value[key], str):
            raise _InputError(f"{key} must be a string")
    if "executable" in value and value["executable"] is not None and not isinstance(value["executable"], str):
        raise _InputError("executable must be a string or null")
    if not value["acceptance_criteria"].strip() or not value["attributed_diff"]:
        raise _InputError("acceptance_criteria and attributed_diff are required")
    return value
def main() -> int:
    try:
        value = _arguments(json.loads(sys.stdin.read(), parse_constant=lambda v: (_ for _ in ()).throw(
            _InputError(f"invalid JSON constant: {v}"))))
    except (json.JSONDecodeError, _InputError) as error:
        json.dump({"error": "INVALID_INPUT", "message": str(error)}, sys.stdout)
        sys.stdout.write("\n")
        return 2

    try:
        criteria = {key: value[key] for key in ("grounded", "objective", "acceptance_criteria")}
        packet, encoded = readiness_check.build_packet(
            criteria, value["attributed_diff"], value["worker_report_untrusted"])
        verdict = readiness_check.run_checker(
            packet, encoded, agent=value["agent"], model=value["model"],
            repo_root=value["repo_root"], executable=value.get("executable"))
        if not isinstance(verdict, dict) or verdict.get("verdict") not in {
                "UNMET", "NO_UNMET_FOUND", "UNKNOWN", "UNAVAILABLE"}:
            raise ValueError("invalid readiness verdict")
        output = {"verdict": verdict["verdict"], "criterion": verdict.get("criterion"),
                  "evidence": verdict.get("evidence", [])}
    except Exception:
        output = {"verdict": "UNAVAILABLE", "criterion": None, "evidence": []}
    json.dump(output, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
