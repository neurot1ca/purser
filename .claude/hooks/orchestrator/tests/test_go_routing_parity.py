import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude/hooks/orchestrator"
FIXTURE = ORCH / "tests/fixtures/go_policy/routing.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger
import policy
import project_root


def test_p40_python_oracle_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python codex.Pre synchronous reply recovery"
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    for case in fixture["cases"]:
        assert case["name"].startswith("p40:")
        base = tmp_path / case["name"].replace(" ", "_").replace(":", "")
        runtime = base / "runtime"
        repository = base / "repo"
        runtime.mkdir(parents=True)
        repository.mkdir()
        subprocess.run(["git", "init", "-q"], cwd=repository, check=True, timeout=10)
        state = codex._project_state_root(runtime, project_root.project_key(repository))
        sessions = ledger.SessionLedger(state)
        thread = case["thread_id"]
        record = ledger.build_session_record(
            case.get("recorded_task", case["task_id"]), thread,
            case.get("recorded_route", "operation"), "model", "high",
            phase=case.get("recorded_phase", "operation"), scope=["src"], label=case.get("label"),
        )
        if case["kind"] == "corrupt":
            path = sessions.path_for_task(case["task_id"])
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("{corrupt", encoding="utf-8")
        else:
            assert sessions.save(record)["status"] == ledger.OK

        prompt = case.get("prompt")
        if case["kind"] == "mismatch":
            prompt = f"ORCH|task={case['task_id']}|phase={case['phase']}|route={case['route']}\nContinue."
        input_data = {"threadId": thread, "prompt": prompt, **case.get("input", {})}
        call = {"session_id": "p40-session", "hook_event_name": "PreToolUse",
                "tool_name": codex.CODEX_REPLY_TOOL, "tool_use_id": case["name"],
                "tool_input": input_data}

        if case["kind"] == "forbidden":
            assert codex._thread_index_path(runtime, thread) is not None
            assert not codex._indexed_reply_root(runtime, thread)[1]
            parsed = policy.parse_orch_prompt(prompt, config)
            assert parsed.valid and "model" in input_data
        elif case["kind"] == "bare":
            found = sessions.find_by_thread_id(thread)
            assert found is not None and found["live_thread"]["label"] == case["label"]
            assert codex._inherited_reply_parse(prompt, found["live_thread"]) is not None
        elif case["kind"] == "corrupt":
            parsed = policy.parse_orch_prompt(prompt, config)
            assert parsed.valid
            assert sessions.find_by_thread_id(thread) is None
            assert sessions.load(case["task_id"])["status"] == ledger.CORRUPT
        else:
            found = sessions.find_by_thread_id(thread)
            assert found is not None
            parsed = policy.parse_orch_prompt(prompt, config)
            assert parsed.valid
            # Python checks task, then phase, then route and returns the first mismatch.
            fields = [("task_id", case["task_id"], "THREAD_TASK_MISMATCH"),
                      ("phase", case["phase"], "THREAD_PHASE_MISMATCH"),
                      ("route", case["route"], "THREAD_ROUTE_MISMATCH")]
            assert next(code for field, requested, code in fields
                        if (found["task_id"] if field == "task_id" else found["live_thread"].get(field)) != requested) == case["expected"]

        # Exercise Pre itself after proving this fixture reaches the intended branch.
        result = codex.pre_tool_use(call, config=config, state_root=runtime, repo_root=repository)
        output = result["hookSpecificOutput"]
        if case["expected"] == "VALID":
            assert "permissionDecision" not in output
            assert "updatedInput" in output
        elif case["expected"] == "INTERNAL_LEDGER_CORRUPT":
            assert "permissionDecision" not in output
            assert "session ledger is unreadable" in output["additionalContext"]
        else:
            assert output["permissionDecision"] == "deny"
            assert output["permissionDecisionReason"].startswith(case["expected"] + ":")
