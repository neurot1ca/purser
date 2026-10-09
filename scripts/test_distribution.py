#!/usr/bin/env python3
"""Smoke-test a packaged Purser plugin on the current native host."""

import hashlib
import http.server
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import platform
import re
import secrets
import shutil
import socketserver
import subprocess
import sys
import threading
import zipfile


class DistributionError(Exception):
    pass


def host_target():
    systems = {"Windows": "windows", "Linux": "linux", "Darwin": "darwin"}
    machines = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
    goos = systems.get(platform.system())
    goarch = machines.get(platform.machine().lower())
    if not goos or not goarch:
        raise DistributionError(f"unsupported host: {platform.system()}/{platform.machine()}")
    suffix = ".exe" if goos == "windows" else ""
    return goos, goarch, f"purser-hook-{goos}-{goarch}{suffix}"


def read_sums(path):
    sums = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64})  (.+)", line)
        if not match or match.group(2) in sums:
            raise DistributionError(f"invalid SHA256SUMS entry: {line!r}")
        sums[match.group(2)] = match.group(1).lower()
    return sums


def verify_archive(artifact_dir, archive_path, sums_path=None):
    sums = read_sums(sums_path or artifact_dir / "SHA256SUMS")
    expected = sums.get(archive_path.name)
    actual = hashlib.sha256(archive_path.read_bytes()).hexdigest()
    if expected is None or expected != actual:
        raise DistributionError(f"archive checksum mismatch: {archive_path.name}")
    return actual, sums


def safe_extract(archive_path, destination, selected_name):
    destination = Path(destination)
    extraction_root = destination.resolve()
    with zipfile.ZipFile(archive_path) as archive:
        selected = None
        members = []
        for info in archive.infolist():
            posix_name = PurePosixPath(info.filename.replace("\\", "/"))
            windows_name = PureWindowsPath(info.filename)
            if (
                not posix_name.parts
                or posix_name.is_absolute()
                or windows_name.is_absolute()
                or windows_name.drive
                or ".." in posix_name.parts
                or ".." in windows_name.parts
            ):
                raise DistributionError(f"unsafe archive member: {info.filename}")
            if ((info.external_attr >> 16) & 0o170000) == 0o120000:
                raise DistributionError(f"archive symlink is not supported: {info.filename}")
            path = (extraction_root / Path(*posix_name.parts)).resolve()
            try:
                path.relative_to(extraction_root)
            except ValueError:
                raise DistributionError(f"unsafe archive member: {info.filename}") from None
            members.append((info, path))
            if info.filename == f"bin/{selected_name}":
                selected = info
        if selected is None:
            raise DistributionError(f"host binary missing from archive: bin/{selected_name}")
        mode = (selected.external_attr >> 16) & 0o777
        if not mode & 0o111:
            raise DistributionError("host binary execute bit missing from archive")
        for info, path in members:
            if info.is_dir():
                path.mkdir(parents=True, exist_ok=True)
                continue
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(archive.read(info))
            file_mode = (info.external_attr >> 16) & 0o777
            if file_mode:
                os.chmod(path, file_mode)
        binary = extraction_root / "bin" / selected_name
        if os.name != "nt" and not binary.stat().st_mode & 0o111:
            raise DistributionError("host binary execute bit was not preserved during extraction")
        return binary


def run(command, *, env, cwd, timeout=60, check=True):
    result = subprocess.run(
        command, cwd=cwd, env=env, text=True, encoding="utf-8", errors="replace",
        capture_output=True, timeout=timeout, check=False,
    )
    if check and result.returncode:
        detail = (result.stderr or result.stdout).strip()
        raise DistributionError(f"command failed ({result.returncode}): {command[0]}\n{detail}")
    return result


def fake_tools(root, env):
    """Put inert coder/reviewer fixtures on PATH for any accidental lookup."""
    bindir = root / "fake-tools"
    bindir.mkdir()
    if os.name == "nt":
        for name in ("codex", "gemini"):
            (bindir / f"{name}.cmd").write_text("@echo off\r\nexit /b 91\r\n", encoding="ascii")
    else:
        for name in ("codex", "gemini"):
            path = bindir / name
            path.write_text("#!/bin/sh\nexit 91\n", encoding="ascii")
            path.chmod(0o755)
    env["PATH"] = str(bindir) + os.pathsep + env.get("PATH", "")


def install_with_claude(archive_path, digest, root, env):
    claude = shutil.which("claude", path=env.get("PATH"))
    if not claude:
        return None, None, "SKIPPED: Claude CLI not found; native plugin installation was not run"
    try:
        version = run([claude, "--version"], cwd=root, env=env).stdout.strip()
    except (DistributionError, OSError, subprocess.TimeoutExpired) as exc:
        return None, None, f"SKIPPED: Claude CLI could not be run: {exc}"
    requests = []

    class ArchiveHandler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            requests.append(self.path)
            if self.path != "/" + archive_path.name:
                self.send_error(404)
                return
            data = archive_path.read_bytes()
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_args):
            pass

    class LocalServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
        daemon_threads = True
        allow_reuse_address = True

    server = LocalServer(("127.0.0.1", 0), ArchiveHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    env["NO_PROXY"] = env["no_proxy"] = "127.0.0.1,localhost"
    marketplace = root / "marketplace"
    (marketplace / ".claude-plugin").mkdir(parents=True)
    (marketplace / ".claude-plugin" / "marketplace.json").write_text(json.dumps({
        "name": "distribution-smoke",
        "owner": {"name": "Purser test"},
        "plugins": [{"name": "purser", "source": {
            "source": "archive",
            "url": f"http://127.0.0.1:{server.server_port}/{archive_path.name}",
            "sha256": digest,
        }}],
    }), encoding="utf-8")
    try:
        add = run([claude, "plugin", "marketplace", "add", str(marketplace), "--scope", "user", "--json"],
                  cwd=root, env=env, check=False)
        if add.returncode:
            server.shutdown()
            server.server_close()
            return version, None, "SKIPPED: Claude local marketplace add unavailable: " + (add.stderr or add.stdout).strip()
        install = run([claude, "plugin", "install", "purser@distribution-smoke", "--scope", "user", "--yes", "--json"],
                      cwd=root, env=env, timeout=180, check=False)
        if install.returncode:
            server.shutdown()
            server.server_close()
            return version, None, "SKIPPED: Claude local archive install unavailable: " + (install.stderr or install.stdout).strip()
        if requests != ["/" + archive_path.name]:
            raise DistributionError(f"unexpected archive requests during install: {requests}")
        installed = []
        for candidate in (root / "config").rglob("bin"):
            binary = candidate / host_target()[2]
            if binary.is_file():
                installed.append(binary)
        if len(installed) != 1:
            raise DistributionError(f"expected one installed host binary, found {len(installed)}")
        return version, (installed[0], server), "PASS: Claude plugin install succeeded"
    except (OSError, subprocess.TimeoutExpired) as exc:
        return version, None, f"SKIPPED: Claude local archive install unavailable: {exc}"


def verify_safe_extract_rejects_unsafe_members(root, selected_name):
    cases = (
        ("backslash", r"nested\..\..\escape.txt", 0),
        ("drive", r"C:\escape.txt", 0),
        ("absolute", "/escape.txt", 0),
        ("parent", "../escape.txt", 0),
        ("symlink", "link", 0o120777 << 16),
    )
    for label, member_name, external_attr in cases:
        archive_path = root / f"{label}.zip"
        extraction_root = root / label
        extraction_root.mkdir(parents=True)
        info = zipfile.ZipInfo(member_name)
        info.external_attr = external_attr
        with zipfile.ZipFile(archive_path, "w") as archive:
            archive.writestr(info, b"target")
        try:
            safe_extract(archive_path, extraction_root, selected_name)
        except DistributionError:
            continue
        raise DistributionError(f"unsafe archive member was accepted: {label}")


def verify_mcp(binary, env, cwd):
    process = subprocess.Popen(
        [str(binary), "mcp"], cwd=cwd, env=env,
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, encoding="utf-8", errors="replace",
    )
    try:
        output, _stderr = process.communicate(
            '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}\n'
            '{"jsonrpc":"2.0","method":"notifications/initialized"}\n'
            '{"jsonrpc":"2.0","id":2,"method":"ping"}\n', timeout=30,
        )
    except subprocess.TimeoutExpired:
        process.kill()
        process.communicate()
        raise DistributionError("MCP initialize/ping timed out")
    if process.returncode != 0:
        raise DistributionError(f"MCP process exited {process.returncode}")
    messages = [json.loads(line) for line in output.splitlines() if line.strip()]
    replies = {message.get("id"): message.get("result") for message in messages if "id" in message}
    if 1 not in replies or replies[1].get("serverInfo", {}).get("name") != "codex-exec-mcp":
        raise DistributionError("MCP initialize response was invalid")
    if 2 not in replies or replies[2] != {}:
        raise DistributionError("MCP ping response was invalid")


def verify_hook_binary_events(plugin_root, binary, env, cwd, temp_root):
    config = json.loads((plugin_root / "hooks" / "hooks.json").read_text(encoding="utf-8"))
    sources = []
    for module in config.get("modules", []):
        source = (plugin_root / "hooks" / module).resolve()
        if plugin_root.resolve() not in source.parents or not source.is_file():
            raise DistributionError(f"hook module does not resolve inside plugin: {module}")
        sources.append(source.read_text(encoding="utf-8"))
    register = "\n".join(sources)
    enforcement_module = re.search(r"from ['\"](\./mod/enforcement\.js)['\"]", register)
    if not enforcement_module:
        raise DistributionError("configured hook module does not import enforcement")
    enforcement_source = Path(enforcement_module.group(1).removesuffix(".js") + ".ts")
    enforcement_path = (plugin_root / "hooks" / enforcement_source).resolve()
    enforcement = enforcement_path.read_text(encoding="utf-8")
    if "classic.SessionStart" not in register or "classic.UserPromptSubmit" not in register:
        raise DistributionError("lifecycle hooks are missing from configured module")
    if "classic.PreToolUse" not in enforcement or "$.plugin.root}/bin/" not in enforcement:
        raise DistributionError("enforcement hook does not resolve the plugin bin path")
    if "$.plugin.root}/bin/" not in register or "classic.SessionStart" not in register:
        raise DistributionError("lifecycle module does not resolve the plugin bin path")

    state = temp_root / "state"
    state.mkdir()
    env["PURSER_STATE_DIR"] = str(state)
    events = (
        (plugin_root / "hooks" / "register.ts", {
            "hook_event_name": "UserPromptSubmit", "session_id": "distribution-smoke", "cwd": "",
            "prompt": "distribution smoke",
        }, False),
        (enforcement_path, {
            "hook_event_name": "PreToolUse", "session_id": "distribution-smoke", "cwd": str(cwd),
            "tool_name": "Bash", "tool_input": {"command": "true"},
        }, True),
    )
    for _source, event, enforce in events:
        command = [str(binary)] + (["enforce"] if enforce else [])
        completed = subprocess.run(
            command, cwd=cwd, env=env, input=json.dumps(event), text=True,
            encoding="utf-8", errors="replace", capture_output=True, timeout=60, check=False,
        )
        if completed.returncode:
            raise DistributionError(f"hook event {event['hook_event_name']} exited {completed.returncode}: {completed.stderr.strip()}")


def missing_binary_check(plugin_root, node, env, cwd, selected_name):
    isolated = plugin_root.parent / "missing-plugin"
    shutil.copytree(plugin_root / "bin", isolated / "bin")
    binary = isolated / "bin" / selected_name
    binary.unlink(missing_ok=True)
    result = run([node, str(isolated / "bin" / "purser-launcher.js"), "go-mcp"],
                 cwd=cwd, env=env, check=False)
    expected = "Go binary missing" in result.stderr
    shutil.rmtree(isolated)
    if result.returncode == 0 or not expected:
        raise DistributionError("missing binary did not produce the existing launcher error")
def main(argv):
    if len(argv) != 2:
        raise DistributionError("usage: python scripts/test_distribution.py <artifact-dir>")
    artifact_dir = Path(argv[1]).resolve()
    scratch_dir = Path(__file__).resolve().parents[1] / ".tmp"
    scratch_dir.mkdir(parents=True, exist_ok=True)
    archives = sorted(artifact_dir.glob("purser-*.zip"))
    if len(archives) != 1:
        raise DistributionError(f"expected one purser-X.Y.Z.zip in {artifact_dir}")
    archive = archives[0]
    goos, goarch, selected_name = host_target()
    digest, sums = verify_archive(artifact_dir, archive)
    bad_sums = scratch_dir / f".bad-SHA256SUMS-{secrets.token_hex(6)}"
    try:
        entries = (artifact_dir / "SHA256SUMS").read_text(encoding="utf-8")
        bad_sums.write_text(entries.replace(digest, "0" * 64, 1), encoding="utf-8")
        verify_archive(artifact_dir, archive, bad_sums)
        raise DistributionError("bad checksum was accepted")
    except DistributionError as exc:
        if "archive checksum mismatch" not in str(exc):
            raise
    finally:
        bad_sums.unlink(missing_ok=True)

    manifest = json.loads((artifact_dir / "release-manifest.json").read_text(encoding="utf-8"))
    binary_name = "bin/" + selected_name
    if binary_name not in sums or manifest.get("binary_digests", {}).get(binary_name) != sums[binary_name]:
        raise DistributionError(f"host binary digest is not covered by release metadata: {binary_name}")

    node = shutil.which("node")
    if not node:
        raise DistributionError("Node.js is required for launcher checks")
    env = os.environ.copy()
    root = scratch_dir / f"purser-distribution-{secrets.token_hex(6)}"
    root.mkdir()
    try:
        fake_tools(root, env)
        env["CLAUDE_CONFIG_DIR"] = str(root / "config")
        env["HOME"] = str(root)
        if os.name == "nt":
            env["USERPROFILE"] = str(root)
        extracted_root = root / "extracted"
        extracted_root.mkdir()
        verify_safe_extract_rejects_unsafe_members(root / "safe-extract-negative", selected_name)
        extracted_binary = safe_extract(archive, extracted_root, selected_name)
        version, install, install_note = install_with_claude(archive, digest, root, env)
        if install:
            binary, server = install
            plugin_root = binary.parents[1]
        else:
            binary = extracted_binary
            plugin_root = extracted_root
            server = None
        if hashlib.sha256(binary.read_bytes()).hexdigest() != sums[binary_name]:
            raise DistributionError("installed host binary digest differs from package metadata")
        if os.name != "nt" and not os.access(binary, os.X_OK):
            raise DistributionError("installed host binary is not executable")
        claude = shutil.which("claude", path=env.get("PATH"))
        if claude:
            validation = run([claude, "plugin", "validate", "--json", str(plugin_root)],
                              cwd=root, env=env, check=False)
            if validation.returncode:
                raise DistributionError("Claude plugin validation failed: " + (validation.stderr or validation.stdout).strip())

        project = root / "fixture-project"
        project.mkdir()
        proxy_url = "http://127.0.0.1:9"
        env["HTTP_PROXY"] = env["HTTPS_PROXY"] = env["http_proxy"] = env["https_proxy"] = proxy_url
        env["NO_PROXY"] = env["no_proxy"] = "127.0.0.1,localhost"
        verify_mcp(binary, env, project)
        verify_hook_binary_events(plugin_root, binary, env, project, root)
        missing_binary_check(plugin_root, node, env, project, selected_name)
        if hashlib.sha256(binary.read_bytes()).hexdigest() != sums[binary_name]:
            raise DistributionError("host binary changed during first-run checks")
        if server:
            server.shutdown()
            server.server_close()
        print(json.dumps({
            "os": goos,
            "arch": goarch,
            "selected_binary": selected_name,
            "client_version": version,
            "digest": digest,
            "checks": ["archive checksum", "execute bit", "MCP initialize/ping", "configured hook registration references", "direct lifecycle binary event", "direct enforcement binary event", "direct hook events succeeded with proxy environment pointed at dead address", "bad checksum rejected", "missing-binary launcher error", "unsafe archive members rejected"] + (["Claude plugin validation"] if version else []),
            "claude_install": install_note,
        }, separators=(",", ":")))
        return 0
    finally:
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    try:
        raise SystemExit(main(sys.argv))
    except (DistributionError, OSError, ValueError, zipfile.BadZipFile, subprocess.TimeoutExpired) as exc:
        print(json.dumps({"error": str(exc)}, separators=(",", ":")), file=sys.stderr)
        raise SystemExit(1)
