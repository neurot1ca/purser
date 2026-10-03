"""Keep Python-only SessionStart maintenance after context moves to Go."""

import json
import sys

import async_session_maintenance
import maintenance_reports
import project_root
import state_root


def main() -> int:
    try:
        event = json.load(sys.stdin)
        if not isinstance(event, dict) or event.get("hook_event_name") != "SessionStart":
            return 0
        repository, _reason = project_root.resolve_project_root(event)
        if repository is not None:
            maintenance_reports.wake_background_check(repository)
        async_session_maintenance.cleanup_jobs(state_root.default_state_root())
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
