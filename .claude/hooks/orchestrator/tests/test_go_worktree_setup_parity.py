import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "worktree_setup.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger
import project_profile


def _command(code):
    argv = ["python", "-c", code]
    return subprocess.list2cmdline(argv) if os.name == "nt" else shlex.join(argv)


def test_python_worktree_setup_oracle_records_fixture(tmp_path):
    cases = [
        ("success", _command("print('setup ok')"), 5),
        ("nonzero_exit", _command("import sys; print('setup failed'); sys.exit(7)"), 5),
        ("timeout", _command("import time; print('started', flush=True); time.sleep(5)"), 1),
        ("huge_output", _command("import os; os.write(1, b'x' * 4096)"), 5),
        ("missing_command", "purser_missing_setup_command_76136", 5),
    ]
    recorded = []
    for name, command, timeout in cases:
        try:
            ledger._run_worktree_setup(
                tmp_path, project_profile.WorktreeSetup(setup=command, setup_timeout_seconds=timeout)
            )
            expected = {"failed": False}
        except ledger.WorktreeSetupError as exc:
            output = exc.output
            if name == "missing_command" and output:
                output = "<nonempty-shell-error>"
            expected = {
                "failed": True,
                "setupError": {"exitCode": exc.exit_code, "timedOut": exc.timed_out, "output": output},
            }
        recorded.append({"name": name, "command": command, "timeout_seconds": timeout, "expected": expected})
    FIXTURE.write_text(json.dumps({
        "oracle": "Python ledger._run_worktree_setup", "cases": recorded,
    }, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert [case["name"] for case in recorded] == [
        "success", "nonzero_exit", "timeout", "huge_output", "missing_command"
    ]
