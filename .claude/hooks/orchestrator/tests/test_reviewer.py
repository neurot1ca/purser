import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid
from types import SimpleNamespace

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import policy
import project_profile
import reviewer


CONFIG_PATH = Path(__file__).parents[1] / "config.json"


@pytest.fixture()
def config(tmp_path):
    value = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    executable = tmp_path / "agy.exe"
    executable.write_text("fake executable", encoding="utf-8")
    value["reviewer1_executable"] = str(executable.resolve())
    return value


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


@pytest.mark.skipif(os.name != "nt", reason="Windows console flags")
def test_reviewer_launches_and_git_calls_hide_windows_console(monkeypatch):
    monkeypatch.setattr(reviewer, "os", SimpleNamespace(
        name="nt", path=os.path, devnull=os.devnull, environ=os.environ,
    ))
    launch_calls = []
    git_calls = []
    runner = lambda *args, **kwargs: launch_calls.append(kwargs) or subprocess.CompletedProcess(args[0], 0, "", "")
    cwd = Path.cwd()
    for adapter, command in (
        (reviewer.AGY_ADAPTER, ["agy"]),
        (reviewer.OPENCODE_ADAPTER, ["opencode", "--agent", "reviewer"]),
        (reviewer.CODEX_ADAPTER, ["codex"]),
    ):
        adapter.invoke(command, runner=runner, cwd=cwd, timeout=1)

    monkeypatch.setattr(reviewer.subprocess, "run", lambda *args, **kwargs: git_calls.append(kwargs)
                        or subprocess.CompletedProcess(args[0], 0, "diff", ""))
    repo = Path(__file__).parents[1]
    reviewer._run_git(repo, ["status"])
    reviewer._new_file_diff(repo, "reviewer.py")

    assert len(launch_calls) == 3
    assert all(call["creationflags"] & 0x08000000 for call in launch_calls[1:])
    assert len(git_calls) == 2
    assert all(call["creationflags"] & 0x08000000 for call in git_calls)


@pytest.mark.skipif(os.name != "nt", reason="Windows console flags")
def test_agy_windows_launch_uses_hidden_new_console(monkeypatch):
    class StartupInfo:
        dwFlags = 0
        wShowWindow = 0

    monkeypatch.setattr(reviewer, "os", SimpleNamespace(
        name="nt", path=os.path, devnull=os.devnull, environ=os.environ,
    ))
    monkeypatch.setattr(reviewer.subprocess, "STARTUPINFO", StartupInfo, raising=False)
    monkeypatch.setattr(reviewer.subprocess, "STARTF_USESHOWWINDOW", 1, raising=False)
    monkeypatch.setattr(reviewer.subprocess, "SW_HIDE", 0, raising=False)
    launch_calls = []

    def popen(command, **kwargs):
        launch_calls.append(kwargs)
        return subprocess.CompletedProcess(command, 0, "", "")

    monkeypatch.setattr(reviewer.subprocess, "Popen", popen)
    reviewer.AGY_ADAPTER.invoke(
        ["agy"], runner=reviewer.subprocess.Popen, cwd=Path.cwd(), timeout=1
    )

    assert len(launch_calls) == 1
    launch = launch_calls[0]
    assert launch["creationflags"] == 0x00000010  # CREATE_NEW_CONSOLE
    assert isinstance(launch["startupinfo"], StartupInfo)
    assert launch["startupinfo"].dwFlags & 1  # STARTF_USESHOWWINDOW
    assert launch["startupinfo"].wShowWindow == 0  # SW_HIDE
    assert launch["capture_output"] is True


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


def test_authority_slice_includes_multiple_configured_sources(tmp_path):
    repo_root = tmp_path / "repo"
    repo_root.mkdir()
    (repo_root / ".orchestrator").mkdir()
    profile_toml = repo_root / ".orchestrator" / "profile.toml"
    profile_toml.write_text(
        """schema_version = 1

[authority]
sources = [
    { path = "AGENTS.md", headings = ["Authority", "Working scope", "Proportional validation", "Test and comment quality"] },
    { path = "SPEC.md", headings = ["Corrections to the canonical spec", "Reviewer-1 gate (mechanical)", "Hard constraints"] },
]
""",
        encoding="utf-8",
    )
    agents_md = repo_root / "AGENTS.md"
    agents_md.write_text(
        """# Authority

Observability never takes enforcement down. A hook internal failure fails open
with a visible marker and forces FULL review.

## Working scope

Only the delegated diff is in scope.

## Proportional validation

Validation matches the risk.

## Test and comment quality

Tests must be focused.
""",
        encoding="utf-8",
    )
    spec_md = repo_root / "SPEC.md"
    spec_md.write_text(
        """# Corrections to the canonical spec

No corrections at this time.

## Reviewer-1 gate (mechanical)

The reviewer-1 gate checks for mechanical issues only.

## Hard constraints

No hard constraints defined.
""",
        encoding="utf-8",
    )

    resolved = project_profile.load_profile(
        repo_root, user_config_path=tmp_path / "missing-user-config.toml"
    )

    authority = reviewer._authority_slice(  # noqa: SLF001 - authority seam contract
        repo_root,
        [".claude/hooks/orchestrator/reviewer.py"],
        resolved.authority_sources,
    )

    assert "Observability never takes enforcement down." in authority
    assert "Reviewer-1 gate (mechanical)" in authority


def test_authority_slice_skips_sources_outside_repository():
    scratch = Path(__file__).parents[4] / ".tmp" / f"authority-{uuid.uuid4().hex}"
    repo_root = scratch / "repo"
    repo_root.mkdir(parents=True, exist_ok=True)
    outside = scratch / "outside.md"
    outside.write_text("## Secret\nshould not be read\n", encoding="utf-8")
    try:
        authority = reviewer._authority_slice(  # noqa: SLF001 - authority seam contract
            repo_root,
            [".claude/hooks/orchestrator/reviewer.py"],
            (
                {"path": "../outside.md", "headings": ("Secret",)},
                {"path": str(outside), "headings": ("Secret",)},
            ),
        )

        assert authority == "No readable repository authority slice was available; treat authority as UNKNOWN."
        assert "should not be read" not in authority
    finally:
        shutil.rmtree(scratch)


def run_review(config, runner, supplied=None):
    # Keep test scratch in .tmp, with the reviewer outside the simulated
    # repository it is reviewing.
    scratch = Path(__file__).parents[4] / ".tmp" / f"reviewer1-test-{uuid.uuid4().hex}"
    working_directory = scratch / "reviewer"
    reviewed_root = scratch / "reviewed"
    working_directory.mkdir(parents=True, exist_ok=True)
    reviewed_root.mkdir(parents=True, exist_ok=True)
    try:
        return reviewer.review_materials(
            materials() if supplied is None else supplied,
            config=config,
            runner=runner,
            cwd=working_directory,
            repo_root=reviewed_root,
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


def test_invalid_json_carries_bounded_raw_stdout_and_decoder_detail(config):
    stdout = "x" * 262144 + "{invalid json"
    result = run_review(config, FakeRunner(stdout=stdout))
    assert_unavailable(result)
    assert result["error_code"] == "REVIEWER_RESPONSE_INVALID"
    assert result.raw_stdout == stdout[-262144:]
    assert result.decoder_detail
    assert len(result.decoder_detail) <= 200
    assert "raw_stdout" not in dict(result)


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
        "reviewer1_model_requested": "gemini-3.8-flash-high",
        "served_model_attestation": "UNAVAILABLE",
    }
    command = runner.calls[0][0]
    assert command[0] == config["reviewer1_executable"]
    assert command[command.index("--model") + 1] == "gemini-3.8-flash-high"
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


def test_reviewer_executable_resolution_prefers_config_path(config, monkeypatch):
    configured = config["reviewer1_executable"]
    monkeypatch.setattr(
        reviewer.shutil,
        "which",
        lambda command: pytest.fail(f"PATH lookup should not run for {command}"),
    )
    command = reviewer.AGY_ADAPTER.build_command(
        "Review packet", reviewer.REVIEW_SCHEMA, config
    )
    assert command[0] == configured


def test_reviewer_executable_resolution_uses_provider_path_for_other_provider(
    config, monkeypatch, tmp_path
):
    claude = tmp_path / "claude.exe"
    claude.write_text("fake executable", encoding="utf-8")
    monkeypatch.setenv("PATH", str(tmp_path))
    config["reviewer1_agent"] = "claude"

    assert reviewer._reviewer_executable(config, "claude") == str(claude.resolve())


@pytest.mark.parametrize(
    ("provider", "command_name"),
    (("agy", "agy"), ("opencode", "opencode"), ("codex", "codex")),
)
def test_reviewer_executable_resolution_uses_provider_path_command(
    config, monkeypatch, tmp_path, provider, command_name
):
    executable = tmp_path / command_name
    executable.write_text("fake executable", encoding="utf-8")
    path_executable = str(executable.resolve())
    config.pop("reviewer1_executable")
    monkeypatch.setattr(
        reviewer.shutil,
        "which",
        lambda command: path_executable if command == command_name else None,
    )
    adapter = reviewer.REVIEWER_ADAPTERS[provider]
    if provider == "opencode":
        config.update(reviewer1_model_requested="vendor/model-id")
    elif provider == "codex":
        config.update(reviewer1_model_requested="codex-review-model")
    kwargs = {"cwd": tmp_path} if provider != "agy" else {}
    command = adapter.build_command(
        "Review packet", reviewer.REVIEW_SCHEMA, config, **kwargs
    )
    assert command[0] == path_executable


def test_missing_reviewer_path_reports_looked_up_command(config, monkeypatch):
    config.pop("reviewer1_executable")
    monkeypatch.setattr(reviewer.shutil, "which", lambda _command: None)

    result = run_review(config, FakeRunner())

    assert_unavailable(result)
    message = result["findings"][0]["message"]
    assert "ValueError" in message
    assert "agy" in message
    assert "PATH" in message


def test_shipped_config_has_no_reviewer_executable_path():
    shipped = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    assert "reviewer1_executable" not in shipped


def test_no_material_verdict_with_findings_is_rejected(config):
    runner = FakeRunner(
        stdout=envelope(
            response(
                policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                findings=[{"code": "BUG", "message": "A material bug was found."}],
            )
        )
    )

    result = run_review(config, runner)

    assert_unavailable(result)
    assert result["error_code"] == "REVIEWER_RESPONSE_INVALID"


def reviewer_opencode_stream(final_text=None, *, model="vendor/model-id"):
    content = json.dumps(response()) if final_text is None else final_text
    return "\n".join(
        json.dumps(event)
        for event in (
            {"type": "step_start", "providerID": "vendor", "modelID": model.split("/", 1)[-1]},
            {"type": "text", "part": {"text": content}},
            {"type": "step_finish", "part": {"reason": "stop"}},
        )
    )


def test_opencode_command_uses_ask_config_and_isolated_scratch_dir(config, tmp_path):
    opencode_executable = str((tmp_path / "opencode.exe").resolve())
    opencode_config = {
        **config,
        "reviewer1_executable": opencode_executable,
        "reviewer1_model_requested": "vendor/model-id",
        "reviewer1_agent": "reviewer",
    }

    class InspectingRunner(FakeRunner):
        def __call__(self, command, **kwargs):
            opencode_dir = Path(command[command.index("--dir") + 1])
            prompt = command[-1]
            packet_path = Path(prompt.split("REVIEW_PACKET_PATH=", 1)[1])
            assert packet_path.parent == opencode_dir
            assert packet_path.name == "review-packet.json"
            assert sorted(path.name for path in opencode_dir.iterdir()) == [packet_path.name]
            self.isolated_dir = opencode_dir
            return super().__call__(command, **kwargs)

    runner = InspectingRunner(stdout=reviewer_opencode_stream())
    packet = reviewer.build_review_packet(
        materials(), config["reviewer1_packet_max_bytes"]
    )
    caller_workdir = tmp_path / "caller-workdir"
    caller_workdir.mkdir()
    (caller_workdir / "must-not-be-visible.txt").write_text("private", encoding="utf-8")
    reviewed_root = tmp_path / "reviewed"
    reviewed_root.mkdir()

    result = reviewer.invoke_packet(
        packet,
        opencode_config,
        runner=runner,
        cwd=caller_workdir,
        repo_root=reviewed_root,
        reviewer_agent="opencode",
    )

    assert isinstance(result, reviewer.CompletedReview), getattr(result, "message", None)
    command, kwargs = runner.calls[0]
    assert command[:2] == [opencode_executable, "run"]
    assert command[command.index("--model") + 1] == "vendor/model-id"
    assert command[command.index("--agent") + 1] == "reviewer"
    assert Path(command[command.index("--dir") + 1]) == runner.isolated_dir
    assert runner.isolated_dir != caller_workdir
    assert not {"--standalone", "--yolo", "--dangerously-skip-permissions"}.intersection(command)
    permissions = json.loads(kwargs["env"]["OPENCODE_CONFIG_CONTENT"])
    confined = {"edit", "write", "bash", "webfetch", "task", "external_directory"}
    assert all(permissions["permission"][key] == "ask" for key in confined)
    assert all(permissions["agent"]["reviewer"]["permission"][key] == "ask" for key in confined)
    assert result["provider"] == "opencode"
    assert result["served_model_attestation"] == "vendor/model-id"
    assert sorted(path.name for path in caller_workdir.iterdir()) == ["must-not-be-visible.txt"]


def test_provider_reviewers_accept_one_json_fence_and_reject_ambiguous_text():
    fenced = "```json\n" + json.dumps(response()) + "\n```"
    parsed = reviewer.OPENCODE_ADAPTER.parse(reviewer_opencode_stream(fenced))
    assert parsed[1]["result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    for invalid in (
        fenced + "\n" + fenced,
        "The review is clean.\n" + json.dumps(response()),
        "```json\n"
        + json.dumps(response(
            policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            findings=[{"code": "BUG", "message": "A material issue exists."}],
        ))
        + "\n```",
    ):
        with pytest.raises(ValueError):
            reviewer.OPENCODE_ADAPTER.parse(reviewer_opencode_stream(invalid))


def test_codex_command_enforces_read_only_execution(config, tmp_path):
    codex_executable = str((tmp_path / "codex.exe").resolve())
    codex_config = {
        **config,
        "reviewer1_executable": codex_executable,
        "reviewer1_model_requested": "codex-review-model",
    }
    command = reviewer.CODEX_ADAPTER.build_command(
        "Review packet", reviewer.REVIEW_SCHEMA, codex_config, cwd=tmp_path
    )

    assert command[:2] == [codex_executable, "exec"]
    assert "--json" in command
    assert command[command.index("-s") + 1] == "read-only"
    assert command[command.index("-c") + 1] == 'approval_policy="never"'
    assert command[command.index("-C") + 1] == str(tmp_path)


def test_codex_adapter_provider_is_reported_on_outcome(config, tmp_path):
    codex_executable = str((tmp_path / "codex.exe").resolve())
    codex_config = {
        **config,
        "reviewer1_executable": codex_executable,
        "reviewer1_model_requested": "codex-review-model",
    }
    stream = "\n".join((
        json.dumps({"type": "item.completed", "item": {
            "type": "agent_message", "text": json.dumps(response())
        }}),
        json.dumps({"type": "turn.completed"}),
    ))
    runner = FakeRunner(stdout=stream)
    packet = reviewer.build_review_packet(
        materials(), config["reviewer1_packet_max_bytes"]
    )
    working_directory = tmp_path / "work"
    working_directory.mkdir()
    reviewed_root = tmp_path / "reviewed"
    reviewed_root.mkdir()

    result = reviewer.invoke_packet(
        packet,
        codex_config,
        runner=runner,
        cwd=working_directory,
        repo_root=reviewed_root,
        reviewer_agent="codex",
    )

    assert isinstance(result, reviewer.CompletedReview)
    assert result["provider"] == "codex"


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


def test_fast_process_failure_retries_once_with_remaining_timeout(config, monkeypatch):
    class SequenceRunner:
        def __init__(self):
            self.calls = []

        def __call__(self, command, **kwargs):
            self.calls.append((command, kwargs))
            if len(self.calls) == 1:
                return subprocess.CompletedProcess(command, 1, stdout="", stderr="503")
            return subprocess.CompletedProcess(command, 0, stdout=envelope(), stderr="")

    times = iter((0.0, 2.0, 2.0, 2.0))
    monkeypatch.setattr(reviewer, "time", SimpleNamespace(monotonic=lambda: next(times)), raising=False)
    runner = SequenceRunner()

    result = run_review(config, runner)

    assert isinstance(result, reviewer.CompletedReview)
    assert len(runner.calls) == 2
    assert runner.calls[1][1]["timeout"] == config["reviewer1_timeout_seconds"] - 2.0
    assert result.retry_count == 1
    assert result.retry_recovered is True


def test_fast_process_failure_twice_reports_retry_and_second_stderr(config, monkeypatch):
    class SequenceRunner:
        def __init__(self):
            self.calls = []

        def __call__(self, command, **kwargs):
            self.calls.append((command, kwargs))
            return subprocess.CompletedProcess(command, len(self.calls), "", f"stderr-{len(self.calls)}")

    times = iter((0.0, 2.0, 2.0, 4.0))
    monkeypatch.setattr(reviewer, "time", SimpleNamespace(monotonic=lambda: next(times)))
    runner = SequenceRunner()

    result = run_review(config, runner)

    assert result["error_code"] == "REVIEWER_PROCESS_FAILED"
    assert "retry" in result["findings"][0]["message"]
    assert result.stderr_tail == "stderr-2"
    assert result.elapsed_seconds == 4.0
    assert len(runner.calls) == 2
    assert result.retry_count == 1
    assert result.retry_recovered is False


def test_slow_process_failure_does_not_retry(config, monkeypatch):
    times = iter((0.0, 60.0))
    monkeypatch.setattr(reviewer, "time", SimpleNamespace(monotonic=lambda: next(times)))
    runner = FakeRunner(returncode=2)

    result = run_review(config, runner)

    assert result["error_code"] == "REVIEWER_PROCESS_FAILED"
    assert len(runner.calls) == 1
    assert result.elapsed_seconds == 60.0


def test_fast_process_failure_does_not_retry_with_five_seconds_or_less(config, monkeypatch):
    bounded = {**config, "reviewer1_timeout_seconds": 60}
    times = iter((0.0, 56.0))
    monkeypatch.setattr(reviewer, "time", SimpleNamespace(monotonic=lambda: next(times)))
    runner = FakeRunner(returncode=2)

    result = run_review(bounded, runner)

    assert result["error_code"] == "REVIEWER_PROCESS_FAILED"
    assert len(runner.calls) == 1


def test_timeout_captures_elapsed_and_stderr_without_retry(config, monkeypatch):
    times = iter((10.0, 13.5))
    monkeypatch.setattr(reviewer, "time", SimpleNamespace(monotonic=lambda: next(times)))
    runner = FakeRunner(error=subprocess.TimeoutExpired(["agy"], 1, stderr=b"stalled stream"))

    result = run_review(config, runner)

    assert result["error_code"] == "REVIEWER_TIMEOUT"
    assert len(runner.calls) == 1
    assert result.elapsed_seconds == 3.5
    assert result.stderr_tail == "stalled stream"


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


def test_added_sensitive_spans_are_redacted_and_force_full_review(config):
    private_key_marker = "-----BEGIN PRIVATE KEY-----"
    password = "password=VeryRealPassword123"
    diff = diff_for("added.py", "+" + private_key_marker, "+" + password)
    runner = PacketInspectingRunner()

    result = run_review(config, runner, materials(diff))

    assert len(runner.calls) == 1
    assert isinstance(runner.forwarded, dict)
    forwarded_diff = runner.forwarded["delegated_diff"]
    assert forwarded_diff.count(reviewer._DIFF_REDACTION_MARKER) == 2  # noqa: SLF001
    assert private_key_marker not in forwarded_diff
    assert password not in forwarded_diff
    assert "VeryRealPassword123" not in forwarded_diff
    assert "[[REDACTED:" in forwarded_diff
    assert result["result"] == policy.UNCERTAIN
    assert {
        "code": "REVIEW_PACKET_ADDED_SECRET_REDACTED",
        "message": "Added credential-like text was redacted from added.py (2 added-line redactions); forcing FULL Claude review.",
    } in result["findings"]


def test_new_file_credential_like_content_is_redacted_and_reviewed(config):
    runner = PacketInspectingRunner()
    diff = diff_for("new.py", "+" + credential_text(), new_file=True)

    result = run_review(config, runner, materials(diff))

    assert len(runner.calls) == 1
    assert reviewer._DIFF_REDACTION_MARKER in runner.forwarded["delegated_diff"]  # noqa: SLF001
    assert credential_text() not in runner.forwarded["delegated_diff"]
    assert any(
        finding["code"] == "REVIEW_PACKET_ADDED_SECRET_REDACTED"
        for finding in result["findings"]
    )


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


@pytest.mark.parametrize(
    "diff",
    [
        "diff --git a/api_key=VeryRealPassword123.py b/api_key=VeryRealPassword123.py\n"
        "--- a/api_key=VeryRealPassword123.py\n"
        "+++ b/api_key=VeryRealPassword123.py\n"
        "@@ -1 +1 @@\n-old\n+new\n",
        "@@ -0,0 +1 @@\n+password=VeryRealPassword123\n",
    ],
    ids=["credential-in-header", "unknown-current-path"],
)
def test_header_or_unknown_path_secret_still_omits_packet(config, diff):
    runner = FakeRunner()

    result = run_review(config, runner, materials(diff))

    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_added_narrative_marker_still_omits_packet(config):
    runner = FakeRunner()

    result = run_review(
        config,
        runner,
        materials(diff_for("narrative.py", "+hidden reasoning: do not forward")),
    )

    assert result["findings"][0]["code"] == "REVIEW_PACKET_SENSITIVE_CONTENT"
    assert runner.calls == []


def test_clean_diff_adds_no_secret_redaction_finding(config):
    runner = FakeRunner()

    result = run_review(config, runner, materials(diff_for("clean.py", "+ordinary = True")))

    assert len(runner.calls) == 1
    assert all(
        finding["code"] != "REVIEW_PACKET_ADDED_SECRET_REDACTED"
        for finding in result["findings"]
    )


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
        if index == 0:
            assert isinstance(removed, reviewer.PacketOmitted)
            assert removed.code == "REVIEW_PACKET_SENSITIVE_CONTENT"
            assert isinstance(added, reviewer.PacketOmitted)
            assert added.code == "REVIEW_PACKET_SENSITIVE_CONTENT"
        else:
            assert isinstance(removed, reviewer.PacketReady)
            assert matched not in removed.packet["delegated_diff"]
            assert isinstance(added, reviewer.PacketReady)
            assert matched not in added.packet["delegated_diff"]


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
        model="gpt-6-sol",
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
        reviewer._refuse_repository_working_directory(
            reviewer._PACKAGE_ROOT, Path(__file__).parents[4]
        )


def test_add_dir_may_not_grant_access_to_the_repository(config):
    with pytest.raises(reviewer.CommandRefused, match="repository under review"):
        reviewer.build_command(
            "review",
            reviewer.REVIEW_SCHEMA,
            config,
            add_dirs=[Path(__file__).parents[4]],
            repository_root=Path(__file__).parents[4],
        )


def test_reviewer_delegated_diff_with_valid_delta(baseline_repo):
    repo, baseline = baseline_repo
    paths = ["existing.py"]
    delta = {
        "changed_paths": paths,
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {
            "unstaged": {"paths": paths},
            "scoped_manifest": {"paths": []},
        },
    }
    (repo / "existing.py").write_text("modified = True\n", encoding="utf-8")

    rev_paths, diff = reviewer._delegated_diff(repo, delta)  # noqa: SLF001
    assert rev_paths == ["existing.py"]
    assert "modified = True" in diff


@pytest.mark.parametrize("diff", [
    "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n@@ -1,2 +1,2 @@\n-old_name = 1\n-old_name = 2\n+new_name = 1\n+other_name = 2\n",
    "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n@@ -1 +1 @@\n-old_name = 1\n+new_name = 2\n",
])
def test_mechanical_fingerprint_rejects_rename_near_matches(diff):
    result = reviewer._mechanical_fingerprint(diff)  # noqa: SLF001
    assert result["classification"] == "unclassified"
    assert result["fingerprint"] is None


def test_mechanical_fingerprint_classifies_consistent_identifier_rename():
    diff = (
        "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n"
        "@@ -1 +1 @@\n-old_name = 1\n+new_name = 1\n"
    )
    result = reviewer._mechanical_fingerprint(diff)  # noqa: SLF001
    assert result["classification"] == "rename"
    assert result["fingerprint"] == hashlib.sha256(
        b"rename:old_name->new_name:.py"
    ).hexdigest()
    assert (result["changed_file_count"], result["hunk_count"]) == (1, 1)


def test_fingerprint_failure_does_not_change_review_result(monkeypatch, config):
    expected = reviewer.CompletedReview(
        result=policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        validation="PASS",
        findings=(),
        conversation_id="conversation",
        reviewer1_model_requested=reviewer.PINNED_MODEL,
    )
    monkeypatch.setattr(reviewer, "_load_config", lambda _value: config)
    monkeypatch.setattr(reviewer, "_seam_materials", lambda *args, **kwargs: materials())
    monkeypatch.setattr(reviewer, "review_materials", lambda *args, **kwargs: expected)
    monkeypatch.setattr(
        reviewer, "_mechanical_fingerprint",
        lambda _diff, _paths=None: (_ for _ in ()).throw(RuntimeError("fingerprint failed")),
        raising=False,
    )
    result = reviewer.review(
        task_id="task",
        thread_id="thread",
        phase="implementation",
        route="normal",
        model="coder",
        effort="medium",
        repo_root=Path.cwd(),
        delta={},
        classification=None,
    )
    assert result is expected


def test_reviewer_emits_only_mechanical_metadata(tmp_path, monkeypatch, config):
    expected = reviewer.CompletedReview(
        result=policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        validation="PASS",
        findings=(),
        conversation_id="conversation",
        reviewer1_model_requested=reviewer.PINNED_MODEL,
    )
    diff = (
        "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n"
        "@@ -1 +1 @@\n-old_name = 1\n+new_name = 1\n"
    )
    monkeypatch.setattr(reviewer, "_load_config", lambda _value: config)
    monkeypatch.setattr(reviewer, "_seam_materials", lambda *args, **kwargs: {**materials(), "delegated_diff": diff})
    monkeypatch.setattr(reviewer, "review_materials", lambda *args, **kwargs: expected)
    result = reviewer.review(
        task_id="task",
        thread_id="thread",
        project_key="0123456789abcdef",
        state_root=tmp_path,
        phase="implementation",
        route="normal",
        model="coder",
        effort="medium",
        repo_root=Path.cwd(),
        delta={"changed_paths": ["a.py"]},
        classification=None,
    )
    event_path = (
        tmp_path / "telemetry" / "events" / "by_task" / "task.jsonl"
    )
    event, = [json.loads(line) for line in event_path.read_text(encoding="utf-8").splitlines()]
    assert result is expected
    assert event["event_type"] == "MECHANICAL_EDIT_OBSERVED"
    assert event["payload"] == {
        "thread_id": "thread",
        "classification": "rename",
        "fingerprint": hashlib.sha256(b"rename:old_name->new_name:.py").hexdigest(),
        "changed_file_count": 1,
        "hunk_count": 1,
    }
    assert "old_name = 1" not in event_path.read_text(encoding="utf-8")


def test_seam_materials_incorporates_objective_and_criteria(baseline_repo):
    repo, baseline = baseline_repo
    delta = {
        "changed_paths": ["existing.py"],
        "baseline_head": baseline,
        "ambiguous_paths": [],
        "sources": {
            "unstaged": {"paths": ["existing.py"]},
            "scoped_manifest": {"paths": []},
        },
    }
    (repo / "existing.py").write_text("modified = 2\n", encoding="utf-8")
    materials = reviewer._seam_materials(  # noqa: SLF001
        "task-123",
        repo,
        delta,
        classification=None,
        task_objective="Fix the edge margin calculation",
        acceptance_criteria="Verify zero drift on negative payoff",
    )
    assert "Task Objective: Fix the edge margin calculation" in materials["objective"]
    assert "Specific Task Criteria: Verify zero drift on negative payoff" in materials["acceptance_criteria"]
