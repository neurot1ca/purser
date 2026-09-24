#!/usr/bin/env python3
"""Create a Purser plugin release and update the local Claude marketplace."""

import argparse
import datetime as _datetime
import json
import ntpath
import os
import re
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CHANGELOG = Path("CHANGELOG.md")
PLUGIN = Path(".claude-plugin/plugin.json")
VERSION_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")
HEADER_RE = re.compile(r"(?m)^## (\d+\.\d+\.\d+) — (\d{4}-\d{2}-\d{2})\s*$")
RELEASE_SUBJECT_RE = re.compile(r"^purser: release (\d+\.\d+\.\d+) \((.*)\)$")


class ReleaseError(Exception):
    def __init__(self, message, code=1):
        super().__init__(message)
        self.code = code


def git(*args, check=True):
    result = subprocess.run(
        ["git", *map(str, args)], cwd=ROOT, text=True, encoding="utf-8",
        errors="replace", capture_output=True, check=False,
    )
    if check and result.returncode:
        raise ReleaseError(f"git {' '.join(map(str, args))}", result.returncode)
    return result


def run_step(label, command):
    result = subprocess.run(
        command, cwd=ROOT, text=True, encoding="utf-8", errors="replace",
        capture_output=True, check=False,
    )
    if result.returncode:
        tail = (result.stderr or result.stdout).strip().splitlines()[-8:]
        print(f"failed step: {label}", file=sys.stderr)
        if tail:
            print("\n".join(tail), file=sys.stderr)
        raise ReleaseError(label, 1)
    return result


def write_step(label, path, data):
    try:
        path.write_bytes(data)
    except OSError as exc:
        print(f"failed step: {label}\n{exc}", file=sys.stderr)
        raise ReleaseError(label, 1)


def read_status():
    result = subprocess.run(
        ["git", "status", "--porcelain=v1", "-z", "--untracked-files=all"],
        cwd=ROOT, capture_output=True, check=False,
    )
    if result.returncode:
        raise ReleaseError("git status", result.returncode)
    records = result.stdout.split(b"\0")
    entries = []
    i = 0
    while i < len(records) and records[i]:
        record = records[i]
        i += 1
        if len(record) < 4:
            continue
        index_state = chr(record[0])
        worktree_state = chr(record[1])
        name = os.fsdecode(record[3:])
        entries.append((index_state, worktree_state, name))
        if index_state in "RC" or worktree_state in "RC":
            i += 1  # the following NUL record is the original rename path
    return entries


def version_tuple(version):
    match = VERSION_RE.fullmatch(version)
    if not match:
        raise ReleaseError(f"invalid semantic version: {version}", 2)
    return tuple(map(int, match.groups()))


def load_plugin():
    plugin_path = ROOT / PLUGIN
    try:
        raw = plugin_path.read_bytes()
        content = raw.decode("utf-8")
        plugin = json.loads(content)
        version = plugin["version"]
    except (OSError, UnicodeError, ValueError, KeyError, TypeError) as exc:
        raise ReleaseError(f"cannot read {PLUGIN}: {exc}", 2)
    version_tuple(version)
    newline = "\r\n" if "\r\n" in content else "\n"
    return plugin, version, newline


def head_info():
    result = git("show", "-s", "--format=%H%x00%P%x00%s", "HEAD")
    fields = result.stdout.split("\0", 2)
    if len(fields) != 3:
        raise ReleaseError("cannot inspect HEAD", 2)
    return fields[0], fields[1].split()[0] if fields[1].strip() else "", fields[2].strip()


def changelog_header(content, version):
    for match in HEADER_RE.finditer(content):
        if match.group(1) == version:
            end = HEADER_RE.search(content, match.end())
            return match, content[match.end():end.start() if end else len(content)]
    return None, None


def resumable_state(version, plugin_version, content):
    match, section = changelog_header(content, version)
    if not match:
        return None
    commit_hash, parent_hash, subject = head_info()
    release_subject = RELEASE_SUBJECT_RE.fullmatch(subject)
    if not release_subject or release_subject.group(1) != version:
        return False
    if plugin_version != version or not parent_hash:
        return False
    expected = git("rev-parse", "--short=7", parent_hash).stdout.strip()
    if not re.search(rf"\(`{re.escape(expected)}`\)\.", section):
        return False
    return commit_hash


def choose_version(requested, plugin_version, content, args, status_entries):
    if requested:
        version_tuple(requested)
        return requested
    # A failed publish leaves HEAD on the release commit. Treat it as a retry
    # only when the invocation still identifies that release, so an ordinary
    # default-version preview on a just-released checkout plans the next patch.
    head_subject = head_info()[2]
    release_subject = RELEASE_SUBJECT_RE.fullmatch(head_subject)
    if release_subject and release_subject.group(1) == plugin_version:
        header, _ = changelog_header(content, plugin_version)
        changed = {name.replace("\\", "/") for _, _, name in status_entries}
        listed_unchanged = bool(args.paths) and all(
            path.replace("\\", "/") not in changed for path in args.paths
        )
        same_release_title = not args.paths and release_subject.group(2) == args.title
        if header and (listed_unchanged or same_release_title):
            return plugin_version
    major, minor, patch = version_tuple(plugin_version)
    return f"{major}.{minor}.{patch + 1}"


def validate_paths(paths):
    reserved = {str(CHANGELOG).replace("\\", "/"), str(PLUGIN).replace("\\", "/")}
    normalized_paths = []
    for path in paths:
        normalized = path.replace("\\", "/")
        while normalized.startswith("./"):
            normalized = normalized[2:]
        if normalized in reserved or normalized.startswith("/") or ntpath.splitdrive(normalized)[0] or ".." in Path(normalized).parts:
            raise ReleaseError(f"unsafe change path: {path}", 2)
        normalized_paths.append(normalized)
    if len(set(normalized_paths)) != len(normalized_paths):
        raise ReleaseError("--paths contains duplicates", 2)
    return set(normalized_paths)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--paths", nargs="+", help="files to include in the change commit")
    parser.add_argument("--message", help="change commit subject (required with --paths)")
    parser.add_argument("--entry", required=True, help="CHANGELOG bullet text")
    parser.add_argument("--title", required=True, help="release commit title")
    parser.add_argument("--version", help="release version (default: current patch + 1)")
    parser.add_argument("--dry-run", action="store_true", help="print the plan without changing anything")
    parser.add_argument("--claude-bin", default="claude", help="Claude CLI executable")
    args = parser.parse_args(argv)

    if args.paths and not args.message:
        parser.error("--message is required with --paths")
    if args.message and not args.paths:
        parser.error("--message requires --paths")
    if not args.entry.strip() or not args.title.strip():
        parser.error("--entry and --title must not be empty")
    if any("\n" in value or "\r" in value for value in (args.message or "", args.title)):
        parser.error("--message and --title must be single-line commit subjects")

    try:
        paths = validate_paths(args.paths or [])
        plugin, plugin_version, newline = load_plugin()
        changelog_path = ROOT / CHANGELOG
        try:
            changelog_bytes = changelog_path.read_bytes()
            changelog = changelog_bytes.decode("utf-8")
        except (OSError, UnicodeError) as exc:
            raise ReleaseError(f"cannot read {CHANGELOG}: {exc}", 2)
        entries = read_status()
        unexpected_staged = [name for index_state, _, name in entries if index_state not in (" ", "?") and name.replace("\\", "/") not in paths]
        if unexpected_staged:
            raise ReleaseError("staged changes outside --paths: " + ", ".join(unexpected_staged), 2)

        version = choose_version(args.version, plugin_version, changelog, args, entries)
        version_tuple(version)

        existing_header, _ = changelog_header(changelog, version)
        resume = resumable_state(version, plugin_version, changelog) if existing_header else None
        if existing_header:
            if not resume:
                raise ReleaseError(f"CHANGELOG header {version} already exists for a different release commit", 2)
            if args.dry_run:
                print(f"dry-run: resume release {version} at {resume[:7]}")
            else:
                run_updates(args.claude_bin)
            return 0

        if args.paths:
            changed = {name.replace("\\", "/") for _, _, name in entries}
            missing = sorted(path for path in paths if path not in changed)
            if missing:
                raise ReleaseError("listed path has no change: " + ", ".join(missing), 2)

        if args.paths:
            change_hash = "<new change commit>"
        else:
            change_hash = head_info()[0][:7]

        today = _datetime.date.today().isoformat()
        entry_text = args.entry.strip()
        while entry_text.endswith("."):
            entry_text = entry_text[:-1]
        separator = "\r\n" if newline == "\r\n" else "\n"
        version_header = f"## {version} — {today}"

        if args.dry_run:
            print(f"dry-run: change commit {args.message}" if args.paths else f"dry-run: reference HEAD {change_hash}")
            print(f"dry-run: changelog {version_header} / {args.entry.strip()}")
            print(f"dry-run: version {version}")
            print(f"dry-run: release commit purser: release {version} ({args.title})")
            print("dry-run: claude plugin marketplace update purser-local")
            print("dry-run: claude plugin update purser@purser-local")
            return 0

        if args.paths:
            literal_paths = sorted(paths)
            run_step("change commit", ["git", "--literal-pathspecs", "add", "-f", "--", *literal_paths])
            run_step("change commit", ["git", "--literal-pathspecs", "commit", "--only", "-m", args.message, "--", *literal_paths])
            change_hash = head_info()[0][:7]
        print(f"committed {change_hash}")

        entry_line = f"{entry_text} (`{change_hash}`)."
        insertion = version_header + separator + separator + entry_line + separator + separator
        first_header = HEADER_RE.search(changelog)
        if first_header:
            changelog = changelog[:first_header.start()] + insertion + changelog[first_header.start():]
        else:
            changelog = changelog.rstrip("\r\n") + separator + separator + insertion
        write_step("changelog", ROOT / CHANGELOG, changelog.encode("utf-8"))
        print("changelog")

        plugin["version"] = version
        plugin_text = json.dumps(plugin, ensure_ascii=False, indent=2) + newline
        write_step("version", ROOT / PLUGIN, plugin_text.encode("utf-8"))
        print(f"version {version}")

        run_step("release commit", ["git", "--literal-pathspecs", "add", "--", str(CHANGELOG), str(PLUGIN)])
        release_message = f"purser: release {version} ({args.title})"
        run_step("release commit", ["git", "commit", "-m", release_message])
        release_hash = head_info()[0][:7]
        print(f"release commit {release_hash}")

        run_updates(args.claude_bin)
        return 0
    except ReleaseError as exc:
        if exc.code == 2 and not str(exc).startswith("failed step:"):
            print(f"refused: {exc}", file=sys.stderr)
            return 2
        if str(exc) not in ("change commit", "release commit", "marketplace update", "plugin update"):
            print(f"failed step: {exc}", file=sys.stderr)
        return 1 if exc.code != 2 else 2


def run_updates(claude_bin):
    executable = [sys.executable, claude_bin] if str(claude_bin).lower().endswith(".py") else [claude_bin]
    run_step("marketplace update", [*executable, "plugin", "marketplace", "update", "purser-local"])
    print("marketplace update ok")
    run_step("plugin update", [*executable, "plugin", "update", "purser@purser-local"])
    print("plugin update ok")


if __name__ == "__main__":
    raise SystemExit(main())
