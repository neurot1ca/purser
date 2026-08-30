import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import sys

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import policy
import reviewer


CONFIG_PATH = Path(__file__).parents[1] / "config.json"


@pytest.fixture()
def config():
    return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))


def git(repo, *args):
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        encoding="utf-8",
        check=True,
    ).stdout.strip()


@pytest.fixture()
def baseline_repo(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    git(repo, "init", "-q")
    (repo / ".gitignore").write_text(
        "ignored.py\nexisting.py\nnew.py\n", encoding="utf-8"
    )
    (repo / "tracked.py").write_text("before\n", encoding="utf-8")
    (repo / "existing.py").write_text("baseline\n", encoding="utf-8")
    git(repo, "add", ".gitignore", "tracked.py")
    git(repo, "add", "-f", "existing.py")
    git(
        repo,
        "-c",
        "user.name=Reviewer Test",
        "-c",
        "user.email=reviewer@example.test",
        "commit",
        "-qm",
        "baseline",
    )
    return repo, git(repo, "rev-parse", "HEAD")


def materials(diff="diff --git a/a.py b/a.py\n-old\n+new\n"):
    return {
        "objective": "Implement the bounded reviewer module.",
        "acceptance_criteria": "The complete delegated diff satisfies the current contract.",
        "authority_contract": "Use the smallest reversible change and focused validation.",
        "delegated_diff": diff,
        "validation_summary": "Focused pytest: PASS (12 passed).",
    }


def credential_text():
    return "api_" + "key = '" + "abcdefghijklmnopqrstuvwxyz012345" + "'"


def diff_for(path, *lines, new_file=False):
    old_path = "/dev/null" if new_file else f"a/{path}"
    old_count = sum(not line.startswith("+") for line in lines)
    new_count = sum(not line.startswith("-") for line in lines)
    old_range = str(0 if old_count == 0 else 1) + (f",{old_count}" if old_count != 1 else "")
    new_range = str(0 if new_count == 0 else 1) + (f",{new_count}" if new_count != 1 else "")
    return (
        f"diff --git a/{path} b/{path}\n"
        + ("new file mode 100644\n" if new_file else "")
        + f"--- {old_path}\n"
        + f"+++ b/{path}\n"
        + f"@@ -{old_range} +{new_range} @@\n"
        + "\n".join(lines)
        + "\n"
    )


def response(result=policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, **changes):
    value = {
        "result": result,
        "validation": "PASS",
        "findings": [],
    }
    value.update(changes)
    return value


def envelope(review_response=None, **changes):
    """Build the envelope the agy CLI actually returns.

    Captured from a live `--output-format json --json-schema` call: `status` is
    upper case, the schema-conforming object arrives in `structured_output`, and
    `response` is free prose that merely quotes it. An earlier version of this
    helper put the payload in `response` with a lower-case status, which no real
    invocation ever produces.
    """

    payload = review_response if review_response is not None else response()
    value = {
        "conversation_id": "conversation-1",
        "status": "SUCCESS",
        "response": f"Here is the review.\n{json.dumps(payload)}\n",
        "structured_output": payload,
        "duration_seconds": 1.25,
        "num_turns": 1,
        "usage": {"input_tokens": 10, "output_tokens": 5},
    }
    value.update(changes)
    return json.dumps(value)


def legacy_envelope(review_response=None, **changes):
    """The older shape, with the payload inline in `response` and no
    `structured_output`. Still accepted as a fallback."""

    value = {
        "conversation_id": "conversation-1",
        "status": "success",
        "response": review_response if review_response is not None else response(),
        "duration_seconds": 1.25,
        "num_turns": 1,
        "usage": {"input_tokens": 10, "output_tokens": 5},
    }
    value.update(changes)
    return json.dumps(value)


class FakeRunner:
    def __init__(self, *, stdout=None, returncode=0, error=None):
        self.stdout = envelope() if stdout is None else stdout
        self.returncode = returncode
        self.error = error
        self.calls = []

    def __call__(self, command, **kwargs):
        self.calls.append((command, kwargs))
        if self.error is not None:
            raise self.error
        return subprocess.CompletedProcess(
            command,
            self.returncode,
            stdout=self.stdout,
            stderr="simulated stderr",
        )


class PacketInspectingRunner(FakeRunner):
    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.forwarded = None
        self.packet_path = None

    def __call__(self, command, **kwargs):
        prompt = command[command.index("--print") + 1]
        marker = "REVIEW_PACKET_PATH="
        self.packet_path = Path(prompt[prompt.index(marker) + len(marker):])
        self.forwarded = json.loads(self.packet_path.read_text(encoding="utf-8"))
        return super().__call__(command, **kwargs)


def assert_unavailable(result):
    assert result["review_status"] == "UNAVAILABLE"
    assert result["available"] is False
    assert result["result"] == policy.REVIEWER1_UNAVAILABLE
    assert result["result"] != policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    assert result["validation"] == "UNKNOWN"
    assert "error_code" in result
    assert "conversation_id" not in result


def run_review(config, runner, supplied=None):
    # Outside the repository on purpose: the reviewer can create files, so
    # running it anywhere inside the worktree under review is refused.
    scratch = Path(tempfile.mkdtemp(prefix="reviewer1-test-"))
    try:
        return reviewer.review_materials(
            materials() if supplied is None else supplied,
            config=config,
            runner=runner,
            cwd=scratch,
        )
    finally:
        shutil.rmtree(scratch, ignore_errors=True)


@pytest.mark.parametrize(
    "runner",
    [
        FakeRunner(returncode=2),
        FakeRunner(error=subprocess.TimeoutExpired(["agy"], 1)),
        FakeRunner(stdout="not json"),
        FakeRunner(stdout=envelope({"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE})),
        FakeRunner(error=FileNotFoundError("agy missing")),
        FakeRunner(returncode=1, stdout="quota exceeded"),
        FakeRunner(stdout=envelope(status="error", review_response="authentication refused")),
        FakeRunner(stdout=json.dumps({"status": "success", "response": response()})),
        FakeRunner(returncode=None),
    ],
    ids=[
        "non-zero-exit",
        "timeout",
        "unparseable-envelope",
        "schema-violation",
        "missing-executable",
        "quota-refusal",
        "auth-refusal",
        "unexpected-envelope-shape",
        "unexpected-process-shape",
    ],
)
def test_every_transport_or_response_failure_is_unavailable(config, runner):
    result = run_review(config, runner)

    assert_unavailable(result)


def test_schema_violation_can_never_become_a_pass(config):
    invalid = response(findings=[{"code": "ONLY_CODE"}])
    runner = FakeRunner(stdout=envelope(invalid))

    result = run_review(config, runner)

    assert_unavailable(result)
    assert result["error_code"] == "REVIEWER_RESPONSE_INVALID"


def test_well_formed_success_parses_and_attests_only_requested_model(config):
    finding = {"code": "BOUNDARY", "message": "Check the observable boundary."}
    runner = PacketInspectingRunner(
        stdout=envelope(
            json.dumps(response(
                policy.CHANGES_REQUIRED,
                validation="FAIL",
                findings=[finding],
            ))
        )
    )

    result = run_review(config, runner)

    assert isinstance(result, reviewer.CompletedReview)
    assert not hasattr(result, "error_code")
    assert dict(result) == {
        "review_status": "COMPLETED",
        "available": True,
        "provider": "agy",
        "result": policy.CHANGES_REQUIRED,
        "validation": "FAIL",
        "findings": [finding],
        "conversation_id": "conversation-1",
        "reviewer1_model_requested": "gemini-3.7-flash-high",
        "served_model_attestation": "UNAVAILABLE",
    }
    command = runner.calls[0][0]
    assert command[0] == r"C:\Users\ugur\scripts\agy.exe"
    assert command[command.index("--model") + 1] == "gemini-3.7-flash-high"
    assert command[command.index("--output-format") + 1] == "json"
    assert command[command.index("--mode") + 1] == "plan"
    assert "--sandbox" in command
    assert "--json-schema" in command
    assert "--disable-slash-commands" not in command
    prompt = command[command.index("--print") + 1]
    assert (
        "absence of orchestrator validation evidence is not by itself grounds for UNCERTAIN"
        in prompt
    )
    assert runner.forwarded == materials()
    assert runner.packet_path is not None and not runner.packet_path.exists()


def test_non_pinned_model_configuration_is_unavailable(config):
    unpinned = dict(config)
    unpinned["reviewer1_model_requested"] = "some-other-model"
    runner = FakeRunner()

    result = run_review(unpinned, runner)

    assert_unavailable(result)
    assert result["error_code"] == "REVIEWER_CONFIG_INVALID"
    assert runner.calls == []


def test_model_reported_unavailable_uses_the_failure_shape(config):
    runner = FakeRunner(stdout=envelope(response(policy.REVIEWER1_UNAVAILABLE)))

    result = run_review(config, runner)

    assert_unavailable(result)
    assert isinstance(result, reviewer.UnavailableReview)
    assert not hasattr(result, "conversation_id")
    assert result["error_code"] == "REVIEWER_MODEL_REPORTED_UNAVAILABLE"


def test_plan_and_disable_slash_commands_are_actively_refused(config):
    with pytest.raises(reviewer.CommandRefused, match="silently voids"):
        reviewer.build_command(
            "review",
            reviewer.REVIEW_SCHEMA,
            config,
            extra_flags=["--disable-slash-commands"],
        )

    with pytest.raises(reviewer.CommandRefused, match="silently voids"):
        reviewer.build_command(
            "review",
            reviewer.REVIEW_SCHEMA,
            config,
            extra_flags=["--disable-slash-commands=true"],
        )


def test_packet_allowlist_rejects_excluded_context_instead_of_forwarding_it(config):
    excluded = {
        **materials(),
        "implementer_narrative": "I think this is correct.",
        "hidden_reasoning": "private chain of thought",
        "claude_conversation": "whole conversation",
        "success_logs": "the entire passing log",
        "repository_state": "all repository files",
        "raw_agent_output": "raw output body",
        "credential": "do not forward",
    }
    runner = FakeRunner()

    result = run_review(config, runner, excluded)

    assert result["review_status"] == "NOT_RUN_INCOMPLETE_PACKET"
    assert result["result"] == policy.UNCERTAIN
    assert result["findings"][0]["code"] == "REVIEW_PACKET_FIELDS_REJECTED"
    assert runner.calls == []


def test_packet_has_exactly_the_five_allowed_fields():
    built = reviewer.build_review_packet(materials(), 100_000)

    assert isinstance(built, reviewer.PacketReady)
    assert set(built.packet) == {
        "objective",
        "acceptance_criteria",
        "authority_contract",
        "delegated_diff",
        "validation_summary",
    }


def test_credential_like_content_is_omitted_without_a_call(config):
    unsafe = materials()
    unsafe["validation_summary"] = "api_" + "key = 'abcdefghijklmnopqrstuvwxyz012345'"
    runner = FakeRunner()

    result = run_review(config, runner, unsafe)

    assert result["result"] == policy.UNCERTAIN
    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_added_credential_like_content_is_omitted_without_a_call(config):
    runner = FakeRunner()

    result = run_review(
        config,
        runner,
        materials(diff_for("added.py", "+" + credential_text())),
    )

    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_new_file_credential_like_content_is_omitted_without_a_call(config):
    runner = FakeRunner()
    diff = diff_for("new.py", "+" + credential_text(), new_file=True)

    result = run_review(config, runner, materials(diff))

    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_removed_credential_like_content_is_redacted_in_ready_packet():
    matched = credential_text()

    built = reviewer.build_review_packet(
        materials(diff_for("removed.py", "-" + matched)), 100_000
    )

    assert isinstance(built, reviewer.PacketReady)
    forwarded = built.packet["delegated_diff"]
    assert reviewer._DIFF_REDACTION_MARKER in forwarded  # noqa: SLF001 - packet contract
    assert matched not in forwarded
    assert "api_key" not in forwarded
    assert "abcdefghijklmnopqrstuvwxyz012345" not in forwarded


def test_context_credential_like_content_is_redacted_in_ready_packet():
    diff = diff_for("context.py", " " + credential_text(), "-before", "+after")

    built = reviewer.build_review_packet(materials(diff), 100_000)

    assert isinstance(built, reviewer.PacketReady)
    assert reviewer._DIFF_REDACTION_MARKER in built.packet[  # noqa: SLF001
        "delegated_diff"
    ]


def test_mixed_added_and_removed_matches_refuse_the_whole_packet(config):
    matched = credential_text()
    runner = FakeRunner()
    diff = diff_for("mixed.py", "-" + matched, "+" + matched)

    result = run_review(config, runner, materials(diff))

    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_redaction_count_and_affected_files_reach_the_packet():
    matched = credential_text()
    diff = diff_for("one.py", "-" + matched) + diff_for(
        "two.py", " " + matched, "-before", "+after"
    )

    built = reviewer.build_review_packet(materials(diff), 100_000)

    assert isinstance(built, reviewer.PacketReady)
    assert built.packet["delegated_diff"].count(reviewer._DIFF_REDACTION_MARKER) == 2
    notice = built.packet["validation_summary"]
    assert "2 forbidden-text span(s)" in notice
    assert "one.py" in notice and "two.py" in notice
    assert "credential-shaped/excluded material" in notice
    assert "not permission to ignore" in notice


def test_all_forbidden_patterns_apply_uniformly_to_diff_content():
    samples = (
        "Implementer " + "narrative: do not forward",
        "-----BEGIN " + "PRIVATE KEY-----",
        "sk-" + "abcdefghijklmnop",
        "AKIA" + "ABCDEFGHIJKLMNOP",
        "Bearer " + "abcdefghijkl",
        credential_text(),
    )

    for index, matched in enumerate(samples):
        removed = reviewer.build_review_packet(
            materials(diff_for(f"removed-{index}.py", "-" + matched)), 100_000
        )
        added = reviewer.build_review_packet(
            materials(diff_for(f"added-{index}.py", "+" + matched)), 100_000
        )
        assert isinstance(removed, reviewer.PacketReady)
        assert matched not in removed.packet["delegated_diff"]
        assert isinstance(added, reviewer.PacketOmitted)
        assert added.code == "REVIEW_PACKET_SENSITIVE_CONTENT"


def test_excluded_narrative_inside_an_allowed_field_is_not_forwarded(config):
    unsafe = materials()
    unsafe["validation_summary"] = "Implementer narrative: trust my conclusion."
    runner = FakeRunner()

    result = run_review(config, runner, unsafe)

    assert isinstance(result, reviewer.IncompleteReview)
    assert result["result"] == policy.UNCERTAIN
    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_oversize_diff_is_never_truncated_or_sent(config):
    bounded = dict(config)
    bounded["reviewer1_packet_max_bytes"] = 200
    complete_diff = "diff --git a/large b/large\n" + ("+complete-line\n" * 100)
    runner = FakeRunner()

    result = reviewer.review_materials(
        materials(complete_diff),
        config=bounded,
        runner=runner,
        cwd=Path(__file__).parent,
    )

    assert result["review_status"] == "NOT_RUN_INCOMPLETE_PACKET"
    assert result["result"] == policy.UNCERTAIN
    assert result["omitted"] == [
        "delegated_diff (complete diff exceeds packet budget; not truncated)"
    ]
    assert "No partial diff was sent" in result["findings"][0]["message"]
    assert runner.calls == []


def test_delegated_diff_excludes_out_of_scope_repository_state(monkeypatch):
    calls = []

    def fake_git(repo_root, args):
        calls.append(args)
        if "--name-only" in args:
            return "delegated.py\n"
        return "diff --git a/delegated.py b/delegated.py\n-value = 1\n+value = 2\n"

    monkeypatch.setattr(reviewer, "_run_git", fake_git)
    delta = {
        "changed_paths": ["delegated.py", "outside.py"],
        "baseline_head": "baseline-head",
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": []}},
    }

    paths, diff = reviewer._delegated_diff(  # noqa: SLF001 - packet boundary
        Path(__file__).parents[4],
        delta,
        {"in_scope_paths": ["delegated.py"]},
    )

    assert paths == ["delegated.py"]
    assert "+value = 2" in diff
    assert "outside.py" not in diff
    assert "outside = 2" not in diff
    assert all(":(literal)outside.py" not in call for call in calls)


def test_new_at_baseline_path_is_included_as_a_complete_addition(baseline_repo):
    repo, baseline = baseline_repo
    (repo / "ignored.py").write_text("first line\nsecond line\n", encoding="utf-8")
    delta = {
        "changed_paths": ["ignored.py"],
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": ["ignored.py"]}},
    }

    paths, diff = reviewer._delegated_diff(repo, delta)  # noqa: SLF001 - packet boundary

    assert paths == ["ignored.py"]
    assert "new file mode" in diff
    assert "--- /dev/null" in diff
    assert "+++ b/ignored.py" in diff
    assert "+first line\n+second line" in diff


def test_manifest_path_that_existed_at_baseline_still_refuses(baseline_repo):
    repo, baseline = baseline_repo
    git(repo, "rm", "--cached", "existing.py")
    (repo / "existing.py").write_text("unrecoverable current content\n", encoding="utf-8")
    delta = {
        "changed_paths": ["existing.py"],
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": ["existing.py"]}},
    }

    with pytest.raises(ValueError, match="no baseline contents.*existing.py"):
        reviewer._delegated_diff(repo, delta)  # noqa: SLF001 - packet boundary


def test_mixed_manifest_delta_refuses_before_reviewing_a_partial_addition(
    baseline_repo, monkeypatch
):
    repo, baseline = baseline_repo
    git(repo, "rm", "--cached", "existing.py")
    (repo / "existing.py").write_text("unrecoverable current content\n", encoding="utf-8")
    (repo / "new.py").write_text("fully known addition\n", encoding="utf-8")
    delta = {
        "changed_paths": ["existing.py", "new.py"],
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": ["existing.py", "new.py"]}},
    }

    def partial_addition(*_args):
        pytest.fail("a mixed incomplete delta must refuse before building any partial addition")

    monkeypatch.setattr(reviewer, "_new_file_diff", partial_addition)
    with pytest.raises(ValueError, match="no baseline contents.*existing.py"):
        reviewer._delegated_diff(repo, delta)  # noqa: SLF001 - packet boundary


def test_seam_validation_statement_reflects_supplied_evidence(baseline_repo):
    repo, baseline = baseline_repo
    (repo / "tracked.py").write_text("after\n", encoding="utf-8")
    delta = {
        "changed_paths": ["tracked.py"],
        "changed_file_count": 1,
        "changed_line_count": 2,
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": []}},
    }

    packet = reviewer._seam_materials(  # noqa: SLF001 - accepted seam contract
        "bounded-task",
        repo,
        delta,
        None,
        "Focused pytest: PASS (4 passed).",
    )

    assert packet["validation_summary"].startswith(
        "Orchestrator-supplied validation evidence: Focused pytest: PASS (4 passed)."
    )
    assert "outcome exposed by the accepted reviewer seam: UNKNOWN" not in packet[
        "validation_summary"
    ]


def test_untracked_or_ignored_delta_without_baseline_content_is_uncertain(config):
    delta = {
        "changed_paths": ["ignored.py"],
        "changed_file_count": 1,
        "changed_line_count": 1,
        "baseline_head": "baseline-head",
        "ambiguous_paths": [],
        "sources": {"scoped_manifest": {"paths": ["ignored.py"]}},
    }

    result = reviewer.review(
        task_id="bounded-task",
        phase="implementation",
        route="normal",
        model="gpt-5.6-sol",
        effort="medium",
        repo_root=Path(__file__).parents[4],
        delta=delta,
        classification={"decision": "PROCEED", "in_scope_paths": ["ignored.py"]},
    )

    assert isinstance(result, reviewer.IncompleteReview)
    assert result["result"] == policy.UNCERTAIN
    assert result["omitted"] == ["delegated_diff"]


def test_live_shaped_envelope_parses(config):
    """Regression for the real CLI shape.

    Before this was fixed the module compared `status` case-sensitively to
    "success", required the envelope key set to match exactly, and read the
    payload out of `response`. Every real invocation therefore raised and the
    reviewer was permanently REVIEWER1_UNAVAILABLE -- a review layer that was
    silently never running.
    """

    runner = FakeRunner(stdout=envelope())
    result = run_review(config, runner)
    assert isinstance(result, reviewer.CompletedReview)
    assert result["result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE


def test_legacy_response_shaped_envelope_still_parses(config):
    runner = FakeRunner(stdout=legacy_envelope())
    result = run_review(config, runner)
    assert isinstance(result, reviewer.CompletedReview)


def test_reviewer_never_runs_inside_the_repository():
    """The reviewer can create files, so its cwd must be outside the worktree."""

    with pytest.raises(reviewer.CommandRefused, match="outside the repository"):
        reviewer._refuse_repository_working_directory(reviewer._PACKAGE_ROOT)


def test_add_dir_may_not_grant_access_to_the_repository(config):
    with pytest.raises(reviewer.CommandRefused, match="repository under review"):
        reviewer.build_command(
            "review",
            reviewer.REVIEW_SCHEMA,
            config,
            add_dirs=[reviewer._REPOSITORY_ROOT],
        )
