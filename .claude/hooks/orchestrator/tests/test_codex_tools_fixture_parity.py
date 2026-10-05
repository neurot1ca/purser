import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[4]
ORACLE = Path(__file__).parent / "fixtures" / "codex_exec_wire"
HOOK_FIXTURE = ROOT / "hooks" / "mod" / "fixtures" / "codex_exec_wire.ts"


def test_codex_tools_fixture_copy_matches_wire_oracle():
    source = [json.loads(path.read_text(encoding="utf-8"))
              for path in sorted(ORACLE.glob("*.json"))]
    module = HOOK_FIXTURE.read_text(encoding="utf-8")
    prefix = "export const CODEX_EXEC_WIRE_FIXTURES = "
    suffix = " as const\n"
    assert module.startswith(prefix) and module.endswith(suffix)
    copied = json.loads(module[len(prefix):-len(suffix)])
    assert copied == source
