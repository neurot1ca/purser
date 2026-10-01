"""Format the Claude instruction for a surfaced stalled job."""

from __future__ import annotations

import time
from typing import Any


def instruction(last_activity_at: Any) -> str:
    minutes = (max(0, int((time.time() - last_activity_at) // 60))
               if isinstance(last_activity_at, (int, float))
               and not isinstance(last_activity_at, bool) else 0)
    receipt = (f"> ⚓ **Purser has seen no worker activity for {minutes} minutes.** "
               "The job is still running; I'm checking its status.")
    return ("Do not call codex_result yet. Restart purser_wait.py for this same jobId "
            "in the background (use the PowerShell tool on Windows); continue waiting. "
            f'Tell the user in the user\'s language exactly one line, translated from: "{receipt}"')
