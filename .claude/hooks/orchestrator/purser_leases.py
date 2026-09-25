"""Inspect or explicitly release durable Purser scope leases."""

from __future__ import annotations

import argparse
import json
import sys

import ledger
import state_root


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument("--list", action="store_true", help="List active leases")
    action.add_argument("--release", metavar="TASK_ID", help="Release this task's active leases")
    args = parser.parse_args(argv)

    runtime_root = state_root.default_state_root()
    entries: list[dict[str, object]] = []
    try:
        paths = sorted((runtime_root / "projects").glob("*/scope_leases.json"))
        for path in paths:
            store = ledger.ScopeLeases(path.parent)
            rows = store.active() if args.list else store.release_task(args.release)
            entries.extend({"project_key": path.parent.name, **row} for row in rows)
    except (OSError, ValueError, TypeError) as error:
        print(f"Lease record could not be read or updated: {error}", file=sys.stderr)
        return 1
    print(json.dumps({"active" if args.list else "released": entries}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
