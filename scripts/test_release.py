import json
import os
import shutil
import subprocess
import sys
import uuid
import importlib.util
from pathlib import Path
from types import SimpleNamespace

import pytest


ROOT = Path(__file__).resolve().parents[1]
RELEASE = ROOT / "scripts" / "release.py"


@pytest.fixture
def tmp_path():
    scratch = ROOT / ".tmp" / f"release-test-{uuid.uuid4().hex}"
    scratch.mkdir(parents=True)
    try:
        yield scratch
    finally:
        shutil.rmtree(scratch, ignore_errors=True)


def run(cmd, cwd, *, env=None, check=True):
    result = subprocess.run(
        cmd, cwd=cwd, env=env, text=True, capture_output=True, check=False
    )
    if check:
        assert result.returncode == 0, result.stdout + result.stderr
    return result


def fake_go(tmp_path):
    source = tmp_path / "fake-go.go"
    source.write_text(
        'package main\nimport ("os"; "strings")\nfunc main() {\n'
        'args := os.Args[1:]; if log := os.Getenv("FAKE_GO_LOG"); log != "" { '
        'f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); '
        '_, _ = f.WriteString(os.Getenv("GOOS")+"/"+os.Getenv("GOARCH")+" "+strings.Join(args, " ")+"\\n"); _ = f.Close() }\n'
        'if os.Getenv("FAKE_GO_FAIL_GOOS") == os.Getenv("GOOS") { os.Exit(9) }; '
        'for i, arg := range args { if arg == "-o" && i+1 < len(args) { _ = os.WriteFile(args[i+1], []byte("fake binary"), 0700); return } }; os.Exit(2) }\n',
        encoding="utf-8",
    )
    bindir = tmp_path / "fake-go-bin"
    bindir.mkdir()
    executable = bindir / ("go.exe" if os.name == "nt" else "go")
    build_env = os.environ.copy()
    build_env["GOCACHE"] = str(tmp_path / "go-cache")
    built = run(["go", "build", "-o", str(executable), str(source)], tmp_path, env=build_env)
    assert built.returncode == 0
    return bindir


def repo(tmp_path):
    path = tmp_path / "repo"
    path.mkdir()
    run(["git", "init", "-q"], path)
    env = os.environ.copy()
    env.update(
        GIT_AUTHOR_NAME="Test Author",
        GIT_AUTHOR_EMAIL="author@example.test",
        GIT_COMMITTER_NAME="Test Committer",
        GIT_COMMITTER_EMAIL="committer@example.test",
        GIT_CONFIG_NOSYSTEM="1",
    )
    (path / ".claude-plugin").mkdir()
    (path / ".claude-plugin" / "plugin.json").write_text(
        '{\n  "name": "purser",\n  "version": "0.1.12"\n}\n', encoding="utf-8"
    )
    (path / "CHANGELOG.md").write_text(
        "# Changelog\n\nPreamble.\n\n## 0.1.12 — 2026-09-24\n\n- Existing entry (`aaaaaaa`).\n",
        encoding="utf-8",
    )
    (path / "base.txt").write_text("base\n", encoding="utf-8")
    (path / "scripts").mkdir()
    shutil.copy2(RELEASE, path / "scripts" / "release.py")
    (path / "go" / "cmd" / "purser-hook").mkdir(parents=True)
    (path / "go" / "go.mod").write_text("module test-hook\n\ngo 1.27\n", encoding="utf-8")
    (path / "go" / "cmd" / "purser-hook" / "main.go").write_text(
        "package main\nfunc main() {}\n", encoding="utf-8"
    )
    (path / "hooks" / "mod").mkdir(parents=True)
    (path / "hooks" / "register.ts").write_text("// mod entry\n", encoding="utf-8")
    (path / "hooks" / "mod" / "read.ts").write_text("// Read module\n", encoding="utf-8")
    fake_bin = fake_go(tmp_path)
    env["PATH"] = str(fake_bin) + os.pathsep + os.environ.get("PATH", "")
    env["GOCACHE"] = str(tmp_path / "go-cache")
    run(["git", "add", "-f", "--", "."], path, env=env)
    run(["git", "commit", "-qm", "initial"], path, env=env)
    return path, env


def fake_claude(tmp_path):
    executable = tmp_path / "fake_claude.py"
    executable.write_text(
        "import os, sys\n"
        "print('fake claude:', ' '.join(sys.argv[1:]))\n"
        "fail = int(os.environ.get('FAKE_CLAUDE_EXIT', '0')) if 'purser@purser-local' in sys.argv else 0\n"
        "if fail:\n"
        "    print('controlled updater failure', file=sys.stderr)\n"
        "sys.exit(fail)\n",
        encoding="utf-8",
    )
    return executable


def release(path, env, claude, *args):
    return run(
        [sys.executable, str(path / "scripts" / "release.py"), "--claude-bin", str(claude),
         "--marketplace-dir", str(path.parent / "marketplace"), *args],
        path,
        env=env,
        check=False,
    )


def test_unrelated_staged_file_is_refused_without_repo_changes(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    (path / "unrelated.txt").write_text("staged elsewhere\n", encoding="utf-8")
    run(["git", "add", "--", "unrelated.txt"], path, env=env)
    before_head = run(["git", "rev-parse", "HEAD"], path).stdout
    before_index = run(["git", "diff", "--cached", "--binary"], path).stdout
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 2
    assert run(["git", "rev-parse", "HEAD"], path).stdout == before_head
    assert run(["git", "diff", "--cached", "--binary"], path).stdout == before_index
    assert (path / "CHANGELOG.md").read_text(encoding="utf-8").count("## 0.1.13") == 0


def test_only_listed_paths_land_in_change_commit(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    (path / "unlisted.txt").write_text("leave for later\n", encoding="utf-8")
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    names = run(["git", "show", "--pretty=format:", "--name-only", "HEAD~1"], path).stdout.splitlines()
    assert names == ["change.txt"]
    assert run(["git", "status", "--porcelain"], path).stdout.strip() == "?? unlisted.txt"


def test_changelog_uses_change_hash_and_inserts_before_newest(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    change_hash = run(["git", "rev-parse", "HEAD~1"], path).stdout.strip()[:7]
    changelog = (path / "CHANGELOG.md").read_text(encoding="utf-8")
    assert changelog.index("## 0.1.13 — ") < changelog.index("## 0.1.12 — ")
    assert f"- **Change:** added change (`{change_hash}`)." in changelog
    assert f"added change. (`{change_hash}`)" not in changelog


def test_failed_plugin_update_resumes_without_new_commits(tmp_path):
    path, env = repo(tmp_path)
    env["FAKE_CLAUDE_EXIT"] = "7"
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    claude = fake_claude(tmp_path)
    args = (
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    failed = release(path, env, claude, *args)
    assert failed.returncode == 1
    assert "plugin update" in failed.stdout + failed.stderr
    assert "controlled updater failure" in failed.stdout + failed.stderr
    after_failure = run(["git", "rev-list", "--count", "HEAD"], path).stdout
    env["FAKE_CLAUDE_EXIT"] = "0"
    resumed = release(path, env, claude, *args)
    assert resumed.returncode == 0, resumed.stdout + resumed.stderr
    assert run(["git", "rev-list", "--count", "HEAD"], path).stdout == after_failure
    assert (path / "CHANGELOG.md").read_text(encoding="utf-8").count("## 0.1.13 — ") == 1


def test_dry_run_changes_nothing(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    before_head = run(["git", "rev-parse", "HEAD"], path).stdout
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
        "--dry-run",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert run(["git", "rev-parse", "HEAD"], path).stdout == before_head
    assert run(["git", "status", "--porcelain"], path).stdout == "?? change.txt\n"
    assert json.loads((path / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8"))["version"] == "0.1.12"
    assert "dry-run" in result.stdout


def test_export_only_uses_committed_tree_and_local_manifest(tmp_path):
    path, env = repo(tmp_path)
    marketplace = tmp_path / "marketplace"
    ignored = path / "ignored.txt"
    (path / ".gitignore").write_text("ignored.txt\n", encoding="utf-8")
    run(["git", "add", "--", ".gitignore"], path, env=env)
    run(["git", "commit", "-qm", "ignore generated file"], path, env=env)
    ignored.write_text("must not ship\n", encoding="utf-8")
    (path / "base.txt").write_text("working copy edit\n", encoding="utf-8")
    claude = fake_claude(tmp_path)
    result = run(
        [sys.executable, str(path / "scripts" / "release.py"), "--export-only",
         "--marketplace-dir", str(marketplace), "--claude-bin", str(claude)],
        path, env=env, check=False,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    exported = marketplace / "purser"
    assert (exported / "base.txt").read_text(encoding="utf-8") == "base\n"
    assert not (exported / "ignored.txt").exists()
    assert (exported / "hooks" / "register.ts").is_file()
    assert (exported / "hooks" / "mod" / "read.ts").is_file()
    manifest = json.loads((marketplace / ".claude-plugin" / "marketplace.json").read_text(encoding="utf-8"))
    assert manifest["name"] == "purser-local"
    assert manifest["owner"]["name"] == "Purser"
    assert manifest["description"] == "Local marketplace for the Purser plugin."
    assert manifest["plugins"] == [{"name": "purser", "source": "./purser"}]
    assert "marketplace update ok" in result.stdout
    assert "plugin update ok" in result.stdout


def test_export_builds_six_go_binaries_in_staged_plugin(tmp_path):
    path, env = repo(tmp_path)
    marketplace = tmp_path / "marketplace"
    log = tmp_path / "go-builds.log"
    env["FAKE_GO_LOG"] = str(log)
    result = release(path, env, fake_claude(tmp_path), "--export-only")
    assert result.returncode == 0, result.stdout + result.stderr
    expected = {
        "windows-amd64.exe", "windows-arm64.exe", "linux-amd64", "linux-arm64",
        "darwin-amd64", "darwin-arm64",
    }
    binaries = marketplace / "purser" / "bin"
    assert {item.name.removeprefix("purser-hook-") for item in binaries.iterdir()} == expected
    builds = log.read_text(encoding="utf-8").splitlines()
    assert len(builds) == 6
    assert {line.split()[0] for line in builds} == {
        "windows/amd64", "windows/arm64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64",
    }
    assert all("build -trimpath -ldflags -s -w -o " in line and "./cmd/purser-hook" in line for line in builds)


def test_staged_export_requires_mod_sources_and_all_six_binaries(tmp_path):
    module_spec = importlib.util.spec_from_file_location("release_under_test", RELEASE)
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    staged = tmp_path / "staged"
    for relative in ("hooks/register.ts", "hooks/mod/read.ts"):
        target = staged / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("source\n", encoding="utf-8")
    for goos, goarch in module.GO_HOOK_TARGETS:
        name = f"purser-hook-{goos}-{goarch}" + (".exe" if goos == "windows" else "")
        target = staged / "bin" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(b"binary")

    module._verify_staged_mod_assets(staged, include_binaries=True)
    (staged / "hooks" / "mod" / "read.ts").unlink()
    with pytest.raises(module.ReleaseError, match="hooks/mod/read.ts"):
        module._verify_staged_mod_assets(staged, include_binaries=True)
    (staged / "hooks" / "mod" / "read.ts").write_text("source\n", encoding="utf-8")
    (staged / "bin" / "purser-hook-darwin-arm64").unlink()
    with pytest.raises(module.ReleaseError, match="purser-hook-darwin-arm64"):
        module._verify_staged_mod_assets(staged, include_binaries=True)


def test_go_hook_build_chmods_four_unix_targets(tmp_path, monkeypatch):
    module_spec = importlib.util.spec_from_file_location("release_under_test", RELEASE)
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    staged_tree = tmp_path / "staged"
    outputs = []
    chmods = []

    def build(command, **_kwargs):
        output = Path(command[command.index("-o") + 1])
        output.write_bytes(b"fake binary")
        outputs.append(output.name)
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setattr(module.shutil, "which", lambda _name: "go")
    monkeypatch.setattr(module.subprocess, "run", build)
    monkeypatch.setattr(module.os, "chmod", lambda path, mode: chmods.append((Path(path).name, mode)))
    module._build_go_hook(staged_tree)

    expected = {"purser-hook-linux-amd64", "purser-hook-linux-arm64", "purser-hook-darwin-amd64", "purser-hook-darwin-arm64"}
    assert set(outputs) == expected | {"purser-hook-windows-amd64.exe", "purser-hook-windows-arm64.exe"}
    assert set(chmods) == {(name, 0o755) for name in expected}


def test_failed_go_build_keeps_previous_marketplace_tree(tmp_path):
    path, env = repo(tmp_path)
    marketplace = tmp_path / "marketplace"
    previous = marketplace / "purser"
    previous.mkdir(parents=True)
    sentinel = previous / "sentinel.txt"
    sentinel.write_text("previous tree\n", encoding="utf-8")
    env["FAKE_GO_FAIL_GOOS"] = "linux"
    updates = fake_claude(tmp_path)
    result = release(path, env, updates, "--export-only")
    assert result.returncode == 1
    assert "Go build failed for linux/amd64" in result.stdout + result.stderr
    assert sentinel.read_text(encoding="utf-8") == "previous tree\n"
    assert not list(marketplace.glob("purser-export-*"))
    assert "marketplace update ok" not in result.stdout


def test_go_hook_launcher_runs_binary_then_falls_back_on_spawn_error(tmp_path):
    plugin = tmp_path / "plugin"
    bindir = plugin / "bin"
    hookdir = plugin / ".claude" / "hooks" / "orchestrator"
    bindir.mkdir(parents=True)
    hookdir.mkdir(parents=True)
    shutil.copy2(ROOT / "bin" / "python-locator.js", bindir / "python-locator.js")
    marker = tmp_path / "python-fallback.txt"
    script = hookdir / "fallback.py"
    script.write_text("# launcher fallback target\n", encoding="utf-8")
    goos, goarch = run(["go", "env", "GOOS", "GOARCH"], ROOT).stdout.splitlines()
    names = {"windows": "windows", "linux": "linux", "darwin": "darwin"}
    suffix = ".exe" if goos == "windows" else ""
    binary = bindir / f"purser-hook-{names[goos]}-{goarch}{suffix}"
    build_env = os.environ.copy()
    build_env["CGO_ENABLED"] = "0"
    build_env["GOCACHE"] = str(tmp_path / "go-cache")
    run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", str(binary), "./cmd/purser-hook"], ROOT / "go", env=build_env)
    payload = json.dumps({
        "hook_event_name": "PreToolUse", "tool_name": "Read", "session_id": "launcher-test-session",
        "tool_input": {"file_path": "sample.txt", "cwd": str(tmp_path)},
    })
    state_dir = tmp_path / "state"
    env = os.environ.copy()
    env["PURSER_STATE_DIR"] = str(state_dir)
    command = ["node", str(bindir / "python-locator.js"), "go-hook", str(script)]
    result = subprocess.run(command, input=payload, text=True, capture_output=True, env=env, check=False)
    assert result.returncode == 0, result.stdout + result.stderr
    journal = state_dir / "context_io" / "turns"
    assert len(list(journal.glob("*.json"))) == 1
    assert not marker.exists()

    binary.unlink()
    assert not binary.exists()
    locator = bindir / "python-locator.js"
    harness = (
        "const fs=require('node:fs'); const locator=require(process.argv[1]); "
        "const script=process.argv[2], marker=process.argv[3], calls=[]; "
        "const code=locator.main(['go-hook',script],()=>({path:'python'}),(command,args)=>{"
        "calls.push([command,args]); if(calls.length===1)return {error:{code:'EPERM'}}; "
        "fs.writeFileSync(marker,command+' '+args.join(' ')); return {status:0};}); "
        "if(code!==0||calls.length!==2||calls[1][1][0]!==script)process.exit(1);"
    )
    fallback = subprocess.run(["node", "-e", harness, str(locator), str(script), str(marker)], text=True, capture_output=True, check=False)
    assert fallback.returncode == 0, fallback.stdout + fallback.stderr
    assert marker.read_text(encoding="utf-8") == f"python {script}"

    retry_harness = (
        "Object.defineProperty(process,'platform',{value:'linux'}); "
        "const fs=require('node:fs'); const locator=require(process.argv[1]); "
        "const script=process.argv[2], calls=[], chmods=[]; fs.chmodSync=(...args)=>chmods.push(args); "
        "let resolves=0; const code=locator.main(['go-hook',script],()=>{resolves++;return {path:'python'}},(command,args)=>{"
        "calls.push([command,args]); return calls.length===1?{error:{code:'EACCES'}}:{status:0};}); "
        "if(code!==0||calls.length!==2||resolves!==0||calls[1][0]!==calls[0][0]||chmods.length!==1||chmods[0][1]!==0o755)process.exit(1);"
    )
    retried = subprocess.run(["node", "-e", retry_harness, str(locator), str(script)], text=True, capture_output=True, check=False)
    assert retried.returncode == 0, retried.stdout + retried.stderr

    chmod_failure_harness = (
        "Object.defineProperty(process,'platform',{value:'linux'}); "
        "const fs=require('node:fs'); const locator=require(process.argv[1]); "
        "const script=process.argv[2], calls=[]; fs.chmodSync=()=>{throw Error('chmod failed')}; "
        "const code=locator.main(['go-hook',script],()=>({path:'python'}),(command,args)=>{"
        "calls.push([command,args]); return calls.length===1?{error:{code:'EACCES'}}:{status:0};}); "
        "if(code!==0||calls.length!==2||calls[1][0]!=='python'||calls[1][1][0]!==script)process.exit(1);"
    )
    chmod_failed = subprocess.run(["node", "-e", chmod_failure_harness, str(locator), str(script)], text=True, capture_output=True, check=False)
    assert chmod_failed.returncode == 0, chmod_failed.stdout + chmod_failed.stderr


def test_failed_export_keeps_previous_tree_and_skips_updates(tmp_path, monkeypatch):
    path, env = repo(tmp_path)
    marketplace = tmp_path / "marketplace"
    previous = marketplace / "purser"
    previous.mkdir(parents=True)
    (previous / "sentinel.txt").write_text("previous\n", encoding="utf-8")
    claude = fake_claude(tmp_path)
    module_spec = importlib.util.spec_from_file_location("release_under_test", path / "scripts" / "release.py")
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    original_run = module.subprocess.run
    updates = []
    monkeypatch.setattr(module, "run_updates", lambda *_: updates.append(True))

    def fail_archive(command, *args, **kwargs):
        if command[:2] == ["git", "archive"]:
            return SimpleNamespace(returncode=2, stderr=b"archive failed", stdout=b"")
        return original_run(command, *args, **kwargs)

    monkeypatch.setattr(module.subprocess, "run", fail_archive)
    result = module.main(["--export-only", "--marketplace-dir", str(marketplace), "--claude-bin", str(claude)])
    assert result == 1
    assert (previous / "sentinel.txt").read_text(encoding="utf-8") == "previous\n"
    assert not list(marketplace.glob("purser-export-*"))
    assert updates == []
