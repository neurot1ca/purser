#!/usr/bin/env python3
"""Create a Purser plugin release and update the local Claude marketplace."""

import argparse
import datetime as _datetime
import io
import json
import ntpath
import os
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CHANGELOG = Path("CHANGELOG.md")
PLUGIN = Path(".claude-plugin/plugin.json")
VERSION_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")
HEADER_RE = re.compile(r"(?m)^## (\d+\.\d+\.\d+) — (\d{4}-\d{2}-\d{2})\s*$")
RELEASE_SUBJECT_RE = re.compile(r"^purser: release (\d+\.\d+\.\d+) \((.*)\)$")
DEFAULT_MARKETPLACE_DIR = Path.home() / ".claude" / "purser" / "marketplace"
GO_HOOK_TARGETS = (
    ("windows", "amd64"), ("windows", "arm64"),
    ("linux", "amd64"), ("linux", "arm64"),
    ("darwin", "amd64"), ("darwin", "arm64"),
)
MOD_SOURCE_FILES = ("hooks/register.ts", "hooks/mod/read.ts", "hooks/mod/lifecycle.ts", "hooks/mod/board.ts", "hooks/mod/usage.ts", "hooks/mod/enforcement.ts")


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


def marketplace_path(value):
    path = Path(value).expanduser().resolve()
    root = ROOT.resolve()
    try:
        inside_repo = os.path.commonpath((str(root), str(path))) == str(root)
    except ValueError:
        inside_repo = False
    if inside_repo:
        raise ReleaseError("--marketplace-dir must be outside the repository", 2)
    return path


def _safe_archive_members(archive, destination):
    members = archive.getmembers()
    for member in members:
        name = Path(member.name)
        if name.is_absolute() or ".." in name.parts:
            raise ReleaseError(f"unsafe path in git archive: {member.name}")
        if member.issym() or member.islnk():
            link = Path(member.linkname)
            if link.is_absolute():
                raise ReleaseError(f"unsafe link in git archive: {member.name}")
            resolved = (destination / name.parent / link).resolve()
            if os.path.commonpath((str(destination.resolve()), str(resolved))) != str(destination.resolve()):
                raise ReleaseError(f"unsafe link in git archive: {member.name}")
    return members


def _extract_archive(archive, destination, members):
    """Extract without tarfile's directory-mode handling, which is unreliable on Windows."""
    directories = []
    for member in members:
        parts = Path(member.name).parts
        output = destination.joinpath(*parts)
        if member.isdir():
            output.mkdir(parents=True, exist_ok=True)
            directories.append((output, member.mode))
            continue
        output.parent.mkdir(parents=True, exist_ok=True)
        if member.issym():
            os.symlink(member.linkname, output)
            continue
        source = archive.extractfile(member)
        if source is None:
            raise ReleaseError(f"cannot read git archive member: {member.name}")
        with source, output.open("wb") as destination_file:
            shutil.copyfileobj(source, destination_file)
        if not member.islnk() and os.name != "nt":
            os.chmod(output, member.mode & 0o777)
    for directory, mode in reversed(directories):
        if os.name != "nt":
            os.chmod(directory, mode & 0o777)


def _build_go_hook(staged_tree):
    go = shutil.which("go")
    if not go:
        raise ReleaseError("Go is required to build marketplace hook binaries")
    binary_dir = staged_tree / "bin"
    binary_dir.mkdir(parents=True, exist_ok=True)
    for goos, goarch in GO_HOOK_TARGETS:
        name = f"purser-hook-{goos}-{goarch}" + (".exe" if goos == "windows" else "")
        output = binary_dir / name
        env = os.environ.copy()
        env.update(CGO_ENABLED="0", GOOS=goos, GOARCH=goarch)
        try:
            result = subprocess.run(
                [go, "build", "-trimpath", "-ldflags", "-s -w", "-o", str(output), "./cmd/purser-hook"],
                cwd=staged_tree / "go", env=env, text=True, encoding="utf-8",
                errors="replace", capture_output=True, check=False,
            )
        except OSError as exc:
            raise ReleaseError(f"Go build failed for {goos}/{goarch}: {exc}")
        if result.returncode:
            detail = (result.stderr or result.stdout).strip()
            raise ReleaseError(f"Go build failed for {goos}/{goarch}" + (f": {detail}" if detail else ""))
        if not output.is_file():
            raise ReleaseError(f"Go build produced no binary for {goos}/{goarch}")
        if goos != "windows":
            os.chmod(output, 0o755)


def export_marketplace(commit, marketplace_dir):
    """Export one committed tree and publish it as the local marketplace source."""
    target = Path(marketplace_dir)
    phase = "prepare"
    try:
        target.mkdir(parents=True, exist_ok=True)
        archive_result = subprocess.run(
            ["git", "archive", "--format=tar", str(commit)], cwd=ROOT,
            capture_output=True, check=False,
        )
        if archive_result.returncode:
            detail = archive_result.stderr.decode("utf-8", errors="replace").strip()
            raise ReleaseError("git archive failed" + (f": {detail}" if detail else ""))

        for _ in range(10):
            stage = target / f"purser-export-{secrets.token_hex(6)}"
            try:
                stage.mkdir()
                break
            except FileExistsError:
                continue
        else:
            raise ReleaseError("cannot allocate marketplace export staging directory")
        staged_tree = stage
        phase = "extract archive"
        with tarfile.open(fileobj=io.BytesIO(archive_result.stdout), mode="r:") as archive:
            members = _safe_archive_members(archive, staged_tree)
            _extract_archive(archive, staged_tree, members)
        _verify_staged_mod_assets(staged_tree, include_binaries=False)
        phase = "build Go hook binaries"
        _build_go_hook(staged_tree)
        _verify_staged_mod_assets(staged_tree, include_binaries=True)
        manifest = {
            "name": "purser-local",
            "description": "Local marketplace for the Purser plugin.",
            "owner": {"name": "Purser"},
            "plugins": [{"name": "purser", "source": "./purser"}],
        }
        live_tree = target / "purser"
        backup = target / f".purser-previous-{os.getpid()}"
        live_manifest_dir = target / ".claude-plugin"
        staged_manifest_dir = target / f".claude-plugin-stage-{os.getpid()}"
        backup_manifest_dir = target / f".claude-plugin-previous-{os.getpid()}"
        if backup.exists() or staged_manifest_dir.exists() or backup_manifest_dir.exists():
            raise ReleaseError("stale marketplace export staging path exists")
        staged_manifest_dir.mkdir()
        (staged_manifest_dir / "marketplace.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        phase = "stage manifest"
        had_live_tree = live_tree.exists()
        had_manifest = live_manifest_dir.exists()
        published_tree = False
        try:
            phase = "backup previous tree"
            if had_live_tree:
                os.replace(live_tree, backup)
            phase = "publish tree"
            os.replace(staged_tree, live_tree)
            published_tree = True
            phase = "backup previous manifest"
            if had_manifest:
                os.replace(live_manifest_dir, backup_manifest_dir)
            phase = "publish manifest"
            os.replace(staged_manifest_dir, live_manifest_dir)
        except OSError:
            if had_manifest and backup_manifest_dir.exists():
                os.replace(backup_manifest_dir, live_manifest_dir)
            if published_tree:
                shutil.rmtree(live_tree, ignore_errors=True)
            if had_live_tree and backup.exists():
                os.replace(backup, live_tree)
            raise
        if backup.exists():
            shutil.rmtree(backup, ignore_errors=True)
        if backup_manifest_dir.exists():
            shutil.rmtree(backup_manifest_dir, ignore_errors=True)
    except (OSError, tarfile.TarError, ReleaseError) as exc:
        if isinstance(exc, ReleaseError):
            raise
        raise ReleaseError(f"marketplace export ({phase}): {exc}")
    finally:
        staged_path = locals().get("staged_manifest_dir")
        if staged_path and staged_path.exists():
            shutil.rmtree(staged_path, ignore_errors=True)
        if "stage" in locals() and stage.exists():
            shutil.rmtree(stage, ignore_errors=True)


def _verify_staged_mod_assets(staged_tree, *, include_binaries):
    required = list(MOD_SOURCE_FILES)
    if include_binaries:
        required.extend(
            f"bin/purser-hook-{goos}-{goarch}" + (".exe" if goos == "windows" else "")
            for goos, goarch in GO_HOOK_TARGETS
        )
    missing = [path for path in required if not (staged_tree / path).is_file()]
    if missing:
        raise ReleaseError("staged export is missing required file(s): " + ", ".join(missing))


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
    parser.add_argument("--entry", help="CHANGELOG bullet text")
    parser.add_argument("--title", help="release commit title")
    parser.add_argument("--version", help="release version (default: current patch + 1)")
    parser.add_argument("--dry-run", action="store_true", help="print the plan without changing anything")
    parser.add_argument("--export-only", action="store_true", help="export HEAD without creating a release")
    parser.add_argument("--marketplace-dir", default=str(DEFAULT_MARKETPLACE_DIR), help="local marketplace directory")
    parser.add_argument("--claude-bin", default="claude", help="Claude CLI executable")
    args = parser.parse_args(argv)

    if args.paths and not args.message:
        parser.error("--message is required with --paths")
    if args.message and not args.paths:
        parser.error("--message requires --paths")
    if args.export_only and (args.paths or args.message or args.version):
        parser.error("--export-only cannot be combined with --paths, --message, or --version")
    if not args.export_only and (not args.entry or not args.entry.strip() or not args.title or not args.title.strip()):
        parser.error("--entry and --title must not be empty")
    if any("\n" in value or "\r" in value for value in (args.message or "", args.title or "")):
        parser.error("--message and --title must be single-line commit subjects")

    try:
        marketplace_dir = marketplace_path(args.marketplace_dir)
        if args.export_only:
            commit = head_info()[0]
            print(f"dry-run: export {commit[:7]} to {marketplace_dir}" if args.dry_run else f"export target {marketplace_dir}")
            if not args.dry_run:
                export_marketplace(commit, marketplace_dir)
                print(f"exported {commit[:7]}")
                run_updates(args.claude_bin)
            return 0

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
                print(f"dry-run: export {resume[:7]} to {marketplace_dir}")
            else:
                export_marketplace(resume, marketplace_dir)
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
            print(f"dry-run: export <release commit> to {marketplace_dir}")
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

        export_marketplace(head_info()[0], marketplace_dir)
        print(f"exported {release_hash}")
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
