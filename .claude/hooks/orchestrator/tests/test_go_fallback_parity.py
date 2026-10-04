import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "fallback.json"
sys.path.insert(0, str(ORCHESTRATOR))
import active_coder_state
from datetime import datetime as RealDateTime, timezone
import codex


def test_p33_python_active_coder_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python active_coder_state.read"
    for case in fixture["cases"]:
        project = tmp_path / case["name"]
        project.mkdir()
        path = active_coder_state.state_path(project, tmp_path / "runtime")
        if case["raw"] is not None:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(case["raw"], encoding="utf-8")
        try:
            actual = active_coder_state.read(project, tmp_path / "runtime")
        except (ValueError, json.JSONDecodeError):
            assert case.get("error") is True, case["name"]
        else:
            assert not case.get("error"), case["name"]
            assert actual == case["expected"], case["name"]


def test_p36_python_pre_fallback_oracle(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))

    class FrozenDateTime:
        @staticmethod
        def now(tz=None):
            return RealDateTime.fromisoformat("2026-10-04T13:00:00+00:00")

        @staticmethod
        def fromisoformat(value):
            return RealDateTime.fromisoformat(value)

    monkeypatch.setattr(codex, "datetime", FrozenDateTime)

    def oracle(case):
        active = case["active"]
        evidence = active.get("triggering_evidence")
        consent = evidence if isinstance(evidence, dict) and "question_session" in evidence else None
        if case.get("fallback") and active.get("active_coder") == "primary":
            if case.get("reason") == "user_override":
                if not (consent is not None and consent.get("question_session") == case["session"]
                        and consent.get("question_shown") is True and consent.get("answerable") is True):
                    return {"action": "deny", "code": "FALLBACK_CONSENT_REQUIRED",
                            "message": "FALLBACK_CONSENT_REQUIRED: reason=user_override requires a shown fallback question and a later user prompt."}
                return {"action": "approve", "kind": "consent"}
            if (consent is not None and consent.get("question_session") == case["session"]
                    and consent.get("answerable") is True):
                return {"action": "decline", "kind": "decline"}
            if consent is not None:
                return {"action": "deny", "code": "FALLBACK_CONSENT_REQUIRED",
                        "message": "FALLBACK_CONSENT_REQUIRED: Wait for the fallback question to be shown and answered before dispatch."}
            quota = case.get("quota") or {}
            trigger = ({"type": "QUOTA", "trigger_id": quota["5h_resets_at"]}
                       if case["phase"] == "implementation" and quota.get("5h_used_pct", 0) >= 95
                       and isinstance(quota.get("5h_resets_at"), str) else case.get("launch"))
            declined = (isinstance(evidence, dict) and evidence.get("declined") is True
                        and trigger is not None and evidence.get("trigger_id") == trigger.get("trigger_id"))
            if case["phase"] == "implementation" and trigger is not None and not declined and case.get("session"):
                kind, question = codex._fallback_question(trigger, case["fallback"], quota)
                return {"action": "question", "code": "FALLBACK_CONSENT_REQUIRED",
                        "message": "A fallback question must be shown before another implementation dispatch.",
                        "kind": kind, "question": question}

        quota = case.get("quota") or {}
        if case.get("reason") != "user_override" and (not active or active.get("active_coder") == "primary"):
            reset = quota.get("5h_resets_at")
            declined = (isinstance(evidence, dict) and evidence.get("declined") is True
                        and evidence.get("type") == "QUOTA" and evidence.get("trigger_id") == reset)
            low = [f"{label}: {quota[label + '_used_pct']:g}% used, resets at {quota[label + '_resets_at']}"
                   for label in ("5h", "7d") if isinstance(quota.get(label + "_used_pct"), (int, float))
                   and quota[label + "_used_pct"] >= 90 and isinstance(quota.get(label + "_resets_at"), str)
                   and not (label == "5h" and declined)]
            if low:
                message = ("Codex quota is nearly exhausted (" + "; ".join(low) + "). "
                           "Tell the user before this task starts; the user may switch the coding agent or wait. "
                           "If the user chooses to proceed with Codex, resend the same call with reason=user_override in the ORCH line.")
                if isinstance(reset, str):
                    message += (" If the user chooses to wait, run a background sleep command until the 5h reset at "
                                + reset + " UTC, then resend the call when it exits; waiting costs nothing.")
                return {"action": "deny", "code": "LOW_CODEX_QUOTA",
                        "message": "LOW_CODEX_QUOTA: " + message}
        return {"action": "continue"}

    for case in fixture["fallback_pre_cases"]:
        assert "p36" in case["name"]
        assert oracle(case) == case["expected"], case["name"]

        if case["expected"]["action"] in {"approve", "decline"}:
            project = tmp_path / case["name"]
            active_coder_state.write(project, {
                "active_coder": case["active"]["active_coder"],
                "triggering_evidence": case["active"]["triggering_evidence"],
                "reset_cycle_marker": None,
                "last_probe_time": None,
            }, tmp_path / "runtime")
            if case["expected"]["action"] == "approve":
                assert active_coder_state.approve_fallback(project, case["session"], tmp_path / "runtime")
                assert active_coder_state.read(project, tmp_path / "runtime")["active_coder"] == "fallback"
            else:
                active_coder_state.update(
                    project,
                    lambda state: state.update(triggering_evidence={
                        "declined": True,
                        "trigger_id": case["active"]["triggering_evidence"]["trigger_id"],
                        "type": case["active"]["triggering_evidence"]["type"],
                    }),
                    tmp_path / "runtime",
                )
                assert active_coder_state.read(project, tmp_path / "runtime")["triggering_evidence"]["declined"] is True
        elif case["name"] == "p36-approval-races-new-question":
            project = tmp_path / case["name"]
            active_coder_state.write(project, {
                "active_coder": "primary",
                "triggering_evidence": case["active"]["triggering_evidence"],
                "reset_cycle_marker": None,
                "last_probe_time": None,
            }, tmp_path / "runtime")
            assert not active_coder_state.approve_fallback(project, case["session"], tmp_path / "runtime")
