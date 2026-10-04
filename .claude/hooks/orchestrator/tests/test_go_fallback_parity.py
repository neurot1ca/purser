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


def test_p37_python_fallback_return_oracle(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))

    def oracle(case):
        if case.get("update_trigger"):
            return "guarded"
        active = case["active"]
        evidence = active.get("triggering_evidence")
        if active.get("active_coder") != "fallback" or not isinstance(evidence, dict):
            return "continue"
        if "question_session" in evidence:
            if (evidence.get("question_session") != case["session"]
                    or evidence.get("question_shown") is not True
                    or evidence.get("answerable") is not True):
                return "deny"
            return "accept" if case["reason"] == "user_override" else "decline"
        if evidence.get("type") == "QUOTA":
            from datetime import datetime
            marker = active.get("reset_cycle_marker")
            reset = (case.get("quota") or {}).get("5h_resets_at")
            if (marker and reset and datetime.fromisoformat(reset.replace("Z", "+00:00"))
                    > datetime.fromisoformat(marker.replace("Z", "+00:00"))
                    and evidence.get("declined_reset_cycle_marker") != reset and case["session"]):
                return "offer"
        if evidence.get("type") in {"CREDIT_EXHAUSTED", "NO_RESPONSE"}:
            return "claim_probe"
        if evidence.get("type") == "RATE_LIMITED":
            delay = evidence.get("retryAfterSeconds", 300)
            if isinstance(delay, bool) or not isinstance(delay, (int, float)) or delay < 0:
                delay = 300
            if float(case["now_epoch"]) >= float(evidence.get("switched_at", "nan")) + delay:
                return "cooldown_return"
        return "continue"

    for case in fixture["fallback_return_cases"]:
        assert "p37" in case["name"]
        from datetime import datetime
        case["now_epoch"] = datetime.fromisoformat(case["now"].replace("Z", "+00:00")).timestamp()
        assert oracle(case) == case["expected"], case["name"]
        project = tmp_path / case["name"]
        runtime = tmp_path / "runtime"
        if case["expected"] == "offer":
            active_coder_state.write(project, case["active"], runtime)
            def record(state):
                evidence = state.get("triggering_evidence")
                trigger = case["active"]["triggering_evidence"]
                if (state.get("active_coder") == "fallback" and isinstance(evidence, dict)
                        and evidence.get("type") == "QUOTA" and evidence.get("trigger_id") == trigger["trigger_id"]
                        and not evidence.get("question_session")):
                    evidence.update(question="quota renewed", question_session=case["session"],
                                   question_reset_marker=case["quota"]["5h_resets_at"],
                                   question_shown=False, answerable=False)
            active_coder_state.update(project, record, runtime)
            assert active_coder_state.read(project, runtime)["triggering_evidence"]["question_session"] == case["session"]
        if case["expected"] in {"accept", "decline"}:
            active_coder_state.write(project, case["active"], runtime)
            evidence = case["active"]["triggering_evidence"]
            changed = active_coder_state.update(
                project,
                lambda state: state.update(active_coder="primary", triggering_evidence=None)
                if state.get("active_coder") == "fallback"
                and state.get("triggering_evidence", {}).get("trigger_id") == evidence["trigger_id"]
                and evidence.get("question_session") == case["session"]
                and evidence.get("question_shown") is True and evidence.get("answerable") is True
                and case["reason"] == "user_override" else None,
                runtime,
            )
            if case["expected"] == "accept":
                assert active_coder_state.read(project, runtime)["active_coder"] == "primary"
            else:
                def decline(state):
                    evidence = state.get("triggering_evidence")
                    if (state.get("active_coder") == "fallback" and isinstance(evidence, dict)
                            and evidence.get("trigger_id") == case["active"]["triggering_evidence"]["trigger_id"]
                            and evidence.get("question_session") == case["session"]
                            and evidence.get("question_shown") is True and evidence.get("answerable") is True):
                        marker = evidence.pop("question_reset_marker", None)
                        for key in ("question", "question_session", "question_shown", "answerable"):
                            evidence.pop(key, None)
                        evidence["declined_reset_cycle_marker"] = marker
                active_coder_state.update(project, decline, runtime)
                result = active_coder_state.read(project, runtime)
                assert result["active_coder"] == "fallback"
                assert "question_session" not in result["triggering_evidence"]
        elif case["expected"] == "claim_probe":
            active_coder_state.write(project, case["active"], runtime)
            interval = 900 if case["active"]["triggering_evidence"]["type"] == "CREDIT_EXHAUSTED" else 300
            def claim():
                claimed = False
                def change(state):
                    nonlocal claimed
                    evidence = state.get("triggering_evidence")
                    if (state.get("active_coder") != "fallback" or not isinstance(evidence, dict)
                            or evidence.get("type") != case["active"]["triggering_evidence"]["type"]
                            or evidence.get("trigger_id") != case["active"]["triggering_evidence"]["trigger_id"]):
                        return
                    try:
                        elapsed = (case["now_epoch"] - float(state["last_probe_time"])
                                   if state.get("last_probe_time") is not None else interval)
                    except (TypeError, ValueError):
                        elapsed = 0
                    if elapsed >= interval:
                        state["last_probe_time"] = str(case["now_epoch"])
                        claimed = True
                active_coder_state.update(project, change, runtime)
                return claimed
            assert claim()
            assert not claim()
        elif case["expected"] == "guarded":
            active_coder_state.write(project, case["active"], runtime)
            changed = False
            def stale_update(state):
                nonlocal changed
                evidence = state.get("triggering_evidence")
                trigger = case["update_trigger"]
                if state.get("active_coder") == "fallback" and isinstance(evidence, dict) \
                        and evidence.get("type") == trigger["type"] and evidence.get("trigger_id") == trigger["trigger_id"]:
                    state["active_coder"] = "primary"
                    changed = True
            active_coder_state.update(project, stale_update, runtime)
            assert not changed
            assert active_coder_state.read(project, runtime) == case["active"]
        elif case["expected"] == "deny":
            active_coder_state.write(project, case["active"], runtime)
            active_coder_state.update(
                project,
                lambda state: state.update(active_coder="primary", triggering_evidence=None)
                if state.get("active_coder") == "fallback"
                and state.get("triggering_evidence", {}).get("question_session") == case["session"]
                else None,
                runtime,
            )
            assert active_coder_state.read(project, runtime) == case["active"]
        elif case["expected"] in {"cooldown_return", "continue"} and case["active"]["triggering_evidence"].get("type") == "RATE_LIMITED":
            active_coder_state.write(project, case["active"], runtime)
            def cooldown(state):
                evidence = state.get("triggering_evidence")
                if (state.get("active_coder") == "fallback" and isinstance(evidence, dict)
                        and evidence.get("type") == "RATE_LIMITED"
                        and evidence.get("trigger_id") == case["active"]["triggering_evidence"]["trigger_id"]
                        and case["now_epoch"] >= float(evidence["switched_at"]) + evidence["retryAfterSeconds"]):
                    state.update(active_coder="primary", triggering_evidence=None,
                                 reset_cycle_marker=None, last_probe_time=None)
            active_coder_state.update(project, cooldown, runtime)
            expected_coder = "primary" if case["expected"] == "cooldown_return" else "fallback"
            assert active_coder_state.read(project, runtime)["active_coder"] == expected_coder
