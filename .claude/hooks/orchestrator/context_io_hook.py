"""Silent, fail-open entrypoint for passive context-I/O lifecycle hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
import sys
import time
from typing import Any

import context_io_telemetry
import project_root


def main() -> int:
    started = time.perf_counter_ns()
    try:
        value: Any = json.load(sys.stdin)
        if isinstance(value, Mapping):
            repository, _reason = project_root.resolve_project_root(value)
            context_io_telemetry.handle(
                value, repo_root=repository, hook_started_ns=started
            )
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
