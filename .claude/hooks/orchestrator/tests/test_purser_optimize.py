import json
import io
import os
from pathlib import Path
import shutil
import sys
import time
import uuid

import pytest


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import purser_optimize as optimize


@pytest.fixture
def tmp_path():
    """Use a unique .tmp workspace because the host temp ACL is unreadable here."""
    scratch = Path(__file__).resolve().parents[4] / ".tmp"
    path = scratch / f"purser-optimize-{uuid.uuid4().hex}"
    path.mkdir()
    try:
        yield path
    finally:
        shutil.rmtree(path)


def rec_user(text="hello", entrypoint="cli", **extra):
    return {"type": "user", "entrypoint": entrypoint, "userType": "external",
            "message": {"role": "user", "content": [{"type": "text", "text": text}]}, **extra}


def rec_assistant(key, *, entrypoint="cli", weight=100, text="ok", tools=(), stop="end_turn"):
    return {"type": "assistant", "entrypoint": entrypoint, "requestId": key,
            "message": {"id": key, "role": "assistant", "stop_reason": stop,
                        "usage": {"input_tokens": weight},
                        "content": ([{"type": "text", "text": text}] if text else []) + list(tools)}}


def write_session(root: Path, name: str, records, *, age_days=0):
    path = root / "project" / f"{name}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(json.dumps(r) if not isinstance(r, str) else r for r in records) + "\n", encoding="utf-8")
    stamp = time.time() - age_days * 86400
    os.utime(path, (stamp, stamp))
    return path


def skill_listing(*rows):
    names = [name for name, _ in rows]
    content = "\n".join(f"- {name}: {desc}" for name, desc in rows)
    return {"type": "attachment", "entrypoint": "cli", "attachment": {
        "type": "skill_listing", "content": content, "names": names,
        "skillCount": len(names), "isInitial": True}}


def mcp_listing(server, description="instructions for a server"):
    block = f"## {server}\n{description}"
    return {"type": "attachment", "entrypoint": "cli", "attachment": {
        "type": "mcp_instructions_delta", "addedNames": [server], "addedBlocks": [block]}}


def tool(name, input=None, id="tool1"):
    return {"type": "tool_use", "id": id, "name": name, "input": input or {}}


def analyze(root, home=None, days=30, project_root=None):
    if root.is_file():
        root = root.parent.parent
    return optimize.analyze(root, days, home=home, project_root=project_root)


def test_unused_plugin_flag_and_used_skill_not_flagged_and_low_cost(tmp_path):
    projects = tmp_path / "projects"
    listing = skill_listing(("unused-plugin:unused", "x" * 2400), ("used-plugin:used", "short"), ("small", "z"))
    records = [listing, rec_user(), rec_assistant("a", tools=[tool("Skill", {"skill": "used"})])]
    report = analyze(write_session(projects, "s", records), tmp_path)
    rows = {row["name"]: row for row in report["startup"]}
    assert rows["plugin:unused-plugin"]["flagged"]
    assert not rows["skill:used-plugin:used"]["flagged"]
    assert not rows["skill:small"]["flagged"]


def test_weighted_totals_dedupe_message_id(tmp_path):
    first = rec_assistant("same", weight=10)
    first["message"]["usage"].update({"cache_creation_input_tokens": 20,
                                      "cache_read_input_tokens": 30, "output_tokens": 4})
    path = write_session(tmp_path / "projects", "s", [
        rec_user(), first, rec_assistant("same", weight=999)])
    report = analyze(path, tmp_path)
    assert report["weighted_tokens"] == 58
    assert report["requests"] == 1


def test_hidden_entrypoint_prompt_signature_and_share(tmp_path):
    projects = tmp_path / "projects"
    path = write_session(projects, "sdk", [
        rec_user("Run security review for sdk-py", "sdk-py", promptSource="sdk"),
        rec_assistant("a", entrypoint="sdk-py", weight=300)])
    report = analyze(path, tmp_path)
    assert report["hidden"][0]["entrypoint"] == "sdk-py"
    assert report["hidden"][0]["prompts"][0]["signature"] == "Run security review for sdk-py"
    assert report["hidden"][0]["share"] == 1


def test_duplicate_skill_sources_in_listing(tmp_path):
    projects = tmp_path / "projects"
    project = tmp_path / "workspace"
    (project / ".claude" / "skills" / "x").mkdir(parents=True)
    listing = skill_listing(("x", "local"), ("plugin:x", "plugin copy"))
    report = analyze(write_session(projects, "s", [listing, rec_user(), rec_assistant("a")]),
                     tmp_path, project_root=project)
    assert any(item["name"] == "x" and set(item["sources"]) == {"project", "plugin"}
               for item in report["duplicate_skills"])


def test_duplicate_skill_under_user_home_and_plugin(tmp_path):
    home = tmp_path / "home"
    (home / ".claude" / "skills" / "review").mkdir(parents=True)
    listing = skill_listing(("plugin:review", "plugin skill"))
    report = analyze(write_session(tmp_path / "projects", "s", [listing, rec_user(), rec_assistant("a")]), home)
    assert any(item["name"] == "review" and set(item["sources"]) == {"user", "plugin"}
               for item in report["duplicate_skills"])


@pytest.mark.parametrize("length,expected", [(800, False), (801, True)])
def test_reply_rule_boundary(tmp_path, monkeypatch, length, expected):
    projects = tmp_path / "projects"
    home = tmp_path / "home"
    write_session(projects, "s", [rec_user(), rec_assistant("a", text="x" * length)])
    monkeypatch.setattr(Path, "home", lambda: home)
    report = analyze(projects, home)
    assert (report["median_reply_chars"] > 800) is expected
    assert ("# Replies" in optimize.render(report)) is expected


def test_reply_heading_already_present(tmp_path, monkeypatch):
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    (home / ".claude" / "CLAUDE.md").write_text("# Replies\nBe concise.\n", encoding="utf-8")
    monkeypatch.setattr(Path, "home", lambda: home)
    projects = tmp_path / "projects"
    write_session(projects, "s", [rec_user(), rec_assistant("a", text="x" * 900)])
    assert "# Replies is present" in optimize.render(analyze(projects, home))


def sequence_session(projects, name):
    records = []
    commands = ("git add -A", "git commit -m update", "claude plugin list")
    for index in range(3):
        records.append(rec_user(f"turn {index}"))
        for step, command in enumerate(commands):
            records.append(rec_assistant(f"{name}-{index}-{step}", weight=10,
                                         tools=[tool("Bash", {"command": command}, f"{name}{index}{step}")]))
    return write_session(projects, name, records)


def test_repeated_three_step_sequence_requires_three_sessions(tmp_path):
    projects = tmp_path / "projects"
    for i in range(3):
        sequence_session(projects, f"session{i}")
    report = analyze(projects, tmp_path)
    assert report["sequences"]
    assert report["sequences"][0]["length"] == 3
    assert report["sequences"][0]["sessions"] == 3
    only_two = tmp_path / "two-projects"
    for i in range(2):
        sequence_session(only_two, f"session{i}")
    assert analyze(only_two, tmp_path)["sequences"] == []


def test_old_files_ignored_and_malformed_lines_skipped(tmp_path):
    projects = tmp_path / "projects"
    write_session(projects, "old", [rec_user(), rec_assistant("old", weight=99)], age_days=90)
    write_session(projects, "fresh", ["not json", rec_user(), rec_assistant("fresh", weight=7)])
    report = analyze(projects, tmp_path, days=30)
    assert report["sessions"] == 1
    assert report["weighted_tokens"] == 7


def test_no_files_written_and_compaction_setting(tmp_path, monkeypatch):
    home = tmp_path / "home"
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir(parents=True)
    config.write_text("[compaction]\nenabled = false\nthreshold_tokens = 123456\n", encoding="utf-8")
    projects = tmp_path / "projects"
    write_session(projects, "s", [rec_user(), rec_assistant("a")])
    before = sorted(str(p.relative_to(tmp_path)) for p in tmp_path.rglob("*"))
    monkeypatch.setattr(Path, "home", lambda: home)
    report = analyze(projects, home)
    after = sorted(str(p.relative_to(tmp_path)) for p in tmp_path.rglob("*"))
    assert before == after
    assert report["compaction"] == {"enabled": False, "threshold_tokens": 123456}
    assert "Compaction advice is off" in optimize.render(report)


def test_mcp_use_and_deferred_names_contribute_startup_cost(tmp_path):
    projects = tmp_path / "projects"
    server = "search-server"
    deferred = {"type": "attachment", "attachment": {"type": "deferred_tools_delta",
                "addedNames": ["mcp__search_server__query"]}}
    records = [mcp_listing(server, "instructions " * 100), deferred, rec_user(),
               rec_assistant("a", tools=[tool("mcp__search_server__query")])]
    report = analyze(write_session(projects, "s", records), tmp_path)
    row = next(row for row in report["startup"] if row["name"] == "mcp:search-server")
    assert sum(row["kind"] == "MCP" for row in report["startup"]) == 1
    assert row["uses"] > 0
    assert row["tokens_per_request"] >= 100
    assert not row["flagged"]


def test_compaction_defaults_on_when_not_configured(tmp_path, monkeypatch):
    home = tmp_path / "home"
    monkeypatch.setattr(Path, "home", lambda: home)
    report = analyze(tmp_path / "projects", home)
    assert report["compaction"]["enabled"] is True
    assert "Compaction advice is on (default)" in optimize.render(report)


def test_plain_output_reconfigures_cp1254_stdout_and_is_ascii(tmp_path, monkeypatch):
    output = io.BytesIO()
    stream = io.TextIOWrapper(output, encoding="cp1254")
    monkeypatch.setattr(optimize.sys, "stdout", stream)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    assert optimize.main(["--projects-root", str(tmp_path / "projects")]) == 0
    assert stream.encoding.lower() == "utf-8"
    assert output.getvalue().decode("utf-8").isascii()


def test_latest_interactive_startup_is_deduped_but_usage_spans_window(tmp_path):
    projects = tmp_path / "projects"
    workspace = tmp_path / "workspace"
    (workspace / ".claude" / "skills" / "local").mkdir(parents=True)
    old_listing = skill_listing(("stale:thing", "old startup"))
    old = [old_listing, rec_user(), rec_assistant(
        "old-use", tools=[tool("Skill", {"skill": "local"}, "old-skill-call")])]
    write_session(projects, "older", old, age_days=2)
    current_listing = skill_listing(("local", "local copy"), ("current:local", "plugin copy"))
    current = [current_listing, current_listing, rec_user(), rec_assistant("current-1"),
               rec_user("next"), rec_assistant("current-2")]
    write_session(projects, "current", current)
    report = analyze(projects, tmp_path, project_root=workspace)
    rows = {item["name"]: item for item in report["startup"]}
    assert "skill:stale:thing" not in rows
    assert rows["skill:local"]["requests_loaded"] == 2
    assert rows["skill:local"]["uses"] == 1
    assert any(item["name"] == "local" and set(item["sources"]) == {"current", "project"}
               for item in report["duplicate_skills"])
    assert all(item["name"] != "thing" for item in report["duplicate_skills"])


def test_builtin_unprefixed_skills_are_not_flagged_or_duplicate_sources(tmp_path):
    projects = tmp_path / "projects"
    listing = skill_listing(("code-review", "x" * 2400), ("plugin:code-review", "plugin copy"))
    report = analyze(write_session(projects, "s", [listing, rec_user(), rec_assistant("a")]), tmp_path,
                     project_root=tmp_path / "workspace")
    rows = {item["name"]: item for item in report["startup"]}
    assert rows["skill:code-review"]["built_in"]
    assert not rows["skill:code-review"]["flagged"]
    assert not any(item["name"] == "code-review" for item in report["duplicate_skills"])


def test_shell_signatures_strip_repeated_navigation_prefixes():
    bash = tool("Bash", {"command": "cd /tmp && cd 'C:/work tree'; python scripts/release.py --check"})
    powershell = tool("PowerShell", {"command": "Set-Location C:/one; pushd C:/two && git status --short"})
    assert optimize._signature([bash]) == ("Bash python scripts/release.py",)
    assert optimize._signature([powershell]) == ("PowerShell git status",)


def _mock_sequence_session(name, commands):
    requests = [{"tools": [tool("Bash", {"command": command}, f"{name}-{index}")], "weight": 10}
                for index, command in enumerate(commands)]
    return {"path": Path(name), "requests": requests,
            "request_turns": [list(range(len(requests)))]}


def test_repeated_ngrams_need_two_signatures_and_count_non_overlapping():
    identical = [_mock_sequence_session(f"same-{index}", ["git add file"] * 6) for index in range(3)]
    assert optimize._sequence_candidates(identical) == []
    mixed = [_mock_sequence_session(f"mixed-{index}", ["git add file", "python run.py"] * 3)
             for index in range(3)]
    candidates = optimize._sequence_candidates(mixed)
    aba = (("Bash git add",), ("Bash python run.py",), ("Bash git add",))
    assert next(item for item in candidates if item["sequence"] == aba)["occurrences"] == 3
    bab = (("Bash python run.py",), ("Bash git add",), ("Bash python run.py",))
    assert next(item for item in candidates if item["sequence"] == bab)["occurrences"] == 3


def test_sequences_require_scriptable_requests_across_three_sessions():
    sessions = []
    for index in range(3):
        read_edit = [{"name": name, "input": {}} for name in ("Read", "Edit", "Edit")]
        grep_sed = [tool("Bash", {"command": command}) for command in
                    ("grep -n pattern file", "sed -n 1,3p file", "sed -n 4,6p file")]
        inline_commands = [
            "python -", "python -c print('x')", "python3 -", "python3 -c print('x')",
            "py -", "py -c print('x')", "node -e console.log('x')",
            "powershell -Command Write-Output x", "pwsh -c Write-Output x",
            "python <<'PY'", "python - <<'PY'",
        ]
        inline = [tool("Bash", {"command": command}) for command in inline_commands]
        scriptable = [tool("Bash", {"command": command}) for command in
                      ("git add -A", "git commit -m message", "claude plugin marketplace list")]
        assert all(not optimize._eligible_signature([item]) for item in inline)
        tools = read_edit + grep_sed + inline + scriptable
        requests = [{"tools": [tool_call], "weight": 10} for tool_call in tools]
        sessions.append({"path": Path(f"eligible-{index}"), "requests": requests,
                         "request_turns": [list(range(len(requests)))]})

    candidates = optimize._sequence_candidates(sessions)
    positive = (("Bash git add",), ("Bash git commit",), ("Bash claude plugin",))
    assert next(item for item in candidates if item["sequence"] == positive)["sessions"] == 3
    assert not any("Read" in str(item["sequence"]) or "Edit" in str(item["sequence"])
                   for item in candidates)
    assert not any("grep" in str(item["sequence"]) or "sed" in str(item["sequence"])
                   for item in candidates)
    assert not any(any(marker in str(item["sequence"]).lower()
                       for marker in ("python -", "python3 -", "py -", "node -e", "powershell -command", "pwsh -c"))
                   for item in candidates)


def test_contained_sequence_keeps_higher_saving_and_large_values_use_m(tmp_path):
    short = (("A",), ("B",), ("A",))
    long = (("A",), ("B",), ("A",), ("C",))
    candidates = [{"sequence": short, "weighted_saving": 100},
                  {"sequence": long, "weighted_saving": 90}]
    assert optimize._select_sequences(candidates) == [candidates[0]]
    assert optimize._format_number(18_022_000) == "18.0M"
    report = analyze(tmp_path / "projects", tmp_path)
    report["sequences"] = [{"sequence": short, "occurrences": 3, "sessions": 3,
                            "weighted_saving": 18_022_000}]
    rendered = optimize.render(report)
    assert "18.0M weighted tokens" in rendered
    assert rendered.isascii()
