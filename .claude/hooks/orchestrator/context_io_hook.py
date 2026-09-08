"""Silent, fail-open entrypoint for passive context-I/O lifecycle hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
from pathlib import Path
import sys
import time
from typing import Any

import context_io_telemetry


REPO_ROOT = Path(__file__).resolve().parents[3]


def main() -> int:
    started = time.perf_counter_ns()
    try:
        value: Any = json.load(sys.stdin)
        if isinstance(value, Mapping):
            context_io_telemetry.handle(
                value, repo_root=REPO_ROOT, hook_started_ns=started
            )
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
