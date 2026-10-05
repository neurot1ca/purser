import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "delta.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger

CASES = [
    {"name": "names", "kind": "names", "raw": "dir\\old.txt\0same\0same\0\0"},
    {"name": "numstat", "kind": "numstat", "raw": "2\t3\tdir\\old.txt\0-\t-\tbinary\0" "1\t0\tdir/old.txt\0"},
    {"name": "numstat_columns", "kind": "numstat", "raw": "bad\0"},
    {"name": "numstat_counts", "kind": "numstat", "raw": "x\t1\tbad.txt\0"},
    {"name": "manifest", "kind": "manifest", "baseline": [
        {"path": "changed", "sha256": "a", "line_count": 2},
        {"path": "removed", "sha256": "b", "line_count": 4},
        {"path": "same", "sha256": "c", "line_count": 9},
    ], "current": [
        {"path": "changed", "sha256": "d", "line_count": 3},
        {"path": "added", "sha256": "e", "line_count": 0},
        {"path": "same", "sha256": "c", "line_count": "invalid"},
    ]},
    {"name": "manifest_counts", "kind": "manifest", "baseline": [], "current": [
        {"path": "bad", "sha256": "a", "line_count": "invalid"},
    ]},
]


def _oracle(case):
    try:
        if case["kind"] == "names":
            result = {"paths": sorted(ledger._parse_name_list(case["raw"].encode()))}
        elif case["kind"] == "numstat":
            result = {"counts": ledger._parse_numstat(case["raw"].encode())}
        else:
            paths, counts = ledger._manifest_delta(case["baseline"], case["current"])
            result = {"paths": sorted(paths), "counts": counts}
        return {**case, "result": result}
    except ledger._InspectionError as error:
        return {**case, "error": {"code": error.code, "message": error.message}}


def test_python_oracle_delta_fixture():
    generated = {
        "oracle": "Python ledger._parse_name_list, _parse_numstat, and _manifest_delta",
        "cases": [_oracle(case) for case in CASES],
    }
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == generated
