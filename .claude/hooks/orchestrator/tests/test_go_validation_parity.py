import json, os, sys
from pathlib import Path
ROOT = Path(__file__).resolve().parents[4]; ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"; FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "worker_validation.json"; sys.path.insert(0, str(ORCHESTRATOR))
import codex
def _cases():
    def record(status="PASS", **extra):
        return "VALIDATION: " + json.dumps({"source": "WORKER_REPORTED", "status": status, **extra}, separators=(",", ":"))
    reports = [
        ("none", None), ("empty", " \n\t"), ("no-validation", "Tests ran; result recorded."),
        ("pass", record(commands=["go test ./internal/enforce/ -run Validation"], exit_codes=[0])),
        ("fail", record("FAIL", commands=["go test ./broken"], exit_codes=[1])), ("unknown", record("UNKNOWN")),
        ("two-lines", record() + "\n" + record("FAIL", commands=["x"], exit_codes=[1])),
        ("empty-payload", "VALIDATION:   "), ("malformed-json", "VALIDATION: {bad"), ("not-object", "VALIDATION: []"),
        ("unknown-keys", record(extra="x")), ("wrong-source", "VALIDATION: " + json.dumps({"source": "OTHER", "status": "PASS", "commands": ["x"], "exit_codes": [0]})),
        ("invalid-status", record("MAYBE")),
        ("missing-summary", "VALIDATION: " + json.dumps({"source": "WORKER_REPORTED", "status": "PASS", "summary": " ", "commands": ["x"], "exit_codes": [0]})),
        ("non-list-commands", "VALIDATION: " + json.dumps({"source": "WORKER_REPORTED", "status": "UNKNOWN", "commands": "x"})),
        ("commands-exit-mismatch", record(commands=["x"], exit_codes=[])), ("non-integer-exit-code", record(commands=["x"], exit_codes=[1.5])),
        ("bool-exit-code", record(commands=["x"], exit_codes=[True])), ("non-list-artifacts", record(artifacts="artifact")),
        ("pass-without-commands", record()), ("pass-with-nonzero", record(commands=["x"], exit_codes=[1])),
        ("fail-without-nonzero", record("FAIL", commands=["x"], exit_codes=[0])), ("oversize-summary", "validation: " + "x" * 2000),
        ("record-too-large", record(summary="x" * 1300, commands=["x"], exit_codes=[0])),
        ("validation-not-at-end", record(commands=["x"], exit_codes=[0]) + "\ntrailing narrative"),
        ("crlf", record(commands=["x"], exit_codes=[0]) + "\r\ncontinued"),
        ("markdown-wrapper", "`" + record(commands=["x"], exit_codes=[0]) + "`"), ("unknown-summary", "narrative Validation: tests uncertain"),
    ]
    return [{"name": name, "report": report, "expected": codex._parse_worker_validation(report)} for name, report in reports]
def test_python_oracle_matches_worker_validation_fixture():
    generated = {"oracle": "codex._parse_worker_validation+_canonical_unknown_validation", "cases": _cases() + [
        {"name": "canonical-none", "canonical": True, "summary": None, "expected": codex._canonical_unknown_validation(None)}]}
    if os.environ.get("WORKER_VALIDATION_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True); FIXTURE.write_text(json.dumps(generated, ensure_ascii=False, separators=(",", ":")) + "\n", encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8")); assert fixture == generated
