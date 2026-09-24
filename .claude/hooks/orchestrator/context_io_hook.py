"""Silent, fail-open entrypoint for passive context-I/O lifecycle hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
import sys
import time
from typing import Any

import context_io_telemetry
import project_profile
import project_root


def main() -> int:
    started = time.perf_counter_ns()
    try:
        stdin_buffer = getattr(sys.stdin, "buffer", None)
        if stdin_buffer is None:
            value: Any = json.load(sys.stdin)
        else:
            value = json.loads(stdin_buffer.read().decode("utf-8", errors="strict"))
        if isinstance(value, Mapping):
            repository, _reason = project_root.resolve_project_root(value)
            protected_paths = (
                project_profile.load_profile(repository).protected_paths
                if repository is not None
                else project_profile._ENGINE_PROTECTED
            )
            context_io_telemetry.handle(
                value,
                repo_root=repository,
                protected_paths=protected_paths,
                hook_started_ns=started,
            )
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
