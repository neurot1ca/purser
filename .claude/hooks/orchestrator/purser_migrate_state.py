"""Copy legacy LOCALAPPDATA state into Purser's current state root."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import os
from pathlib import Path
import shutil
import sys
from typing import Any

import state_root


def _legacy_state_root() -> Path:
    local_app_data = os.environ.get("LOCALAPPDATA")
    if not local_app_data:
        raise ValueError("LOCALAPPDATA is unavailable; cannot locate legacy state")
    return (Path(local_app_data).expanduser() / "claude-agent-orchestrator").resolve()


def _overlaps(left: Path, right: Path) -> bool:
    return left == right or left in right.parents or right in left.parents


def migrate_state(source: Path, destination: Path, *, dry_run: bool = False) -> dict[str, Any]:
    """Merge source into destination without replacing existing destination files."""

    source = source.expanduser().resolve()
    destination = destination.expanduser().resolve()
    result: dict[str, Any] = {
        "source": source,
        "destination": destination,
        "copied": 0,
        "skipped_existing": 0,
        "errors": [],
    }
    if not source.is_dir():
        result["errors"].append(f"legacy state directory is unavailable: {source}")
        return result
    if _overlaps(source, destination):
        result["errors"].append("source and destination overlap; refusing to modify legacy state")
        return result

    if dry_run:
        for current, _directories, files in os.walk(source):
            relative = Path(current).relative_to(source)
            for name in files:
                target = destination / relative / name
                if target.exists():
                    result["skipped_existing"] += 1
                else:
                    result["copied"] += 1
        marker = destination / "MIGRATED_FROM.txt"
        if marker.exists():
            result["skipped_existing"] += 1
        else:
            result["copied"] += 1
        return result

    def copy_without_overwrite(src: str, dst: str) -> str:
        target = Path(dst)
        try:
            output = target.open("xb")
        except FileExistsError:
            result["skipped_existing"] += 1
            return dst
        try:
            with output, open(src, "rb") as input_stream:
                shutil.copyfileobj(input_stream, output)
            shutil.copystat(src, dst)
        except Exception:
            try:
                target.unlink()
            except OSError:
                pass
            raise
        result["copied"] += 1
        return dst

    try:
        shutil.copytree(source, destination, dirs_exist_ok=True, copy_function=copy_without_overwrite)
    except shutil.Error as error:
        result["errors"].extend(str(item) for item in error.args[0])
    except OSError as error:
        result["errors"].append(str(error))

    marker = destination / "MIGRATED_FROM.txt"
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        with marker.open("x", encoding="utf-8", newline="\n") as stream:
            stream.write(f"legacy_path={source}\n")
            stream.write(f"migrated_at_utc={datetime.now(timezone.utc).isoformat()}\n")
        result["copied"] += 1
    except FileExistsError:
        result["skipped_existing"] += 1
    except OSError as error:
        result["errors"].append(f"could not write migration marker: {error}")
    return result


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true", help="show the migration plan without writing files")
    args = parser.parse_args(argv)
    try:
        source = _legacy_state_root()
        destination = state_root.default_state_root()
    except (OSError, ValueError) as error:
        print(f"source: unavailable\ndestination: unavailable\nerrors: 1 ({error})", file=sys.stderr)
        return 1

    result = migrate_state(source, destination, dry_run=args.dry_run)
    print(f"source: {result['source']}")
    print(f"destination: {result['destination']}")
    if args.dry_run:
        print("mode: dry-run (no files written)")
    print(f"copied: {result['copied']}")
    print(f"skipped-existing: {result['skipped_existing']}")
    print(f"errors: {len(result['errors'])}")
    for error in result["errors"]:
        print(f"error: {error}", file=sys.stderr)
    return 1 if result["errors"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
