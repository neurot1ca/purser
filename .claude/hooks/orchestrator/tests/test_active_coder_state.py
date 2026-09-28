from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).parents[1]))
import active_coder_state


def test_concurrent_writers_leave_one_complete_state(tmp_path):
    project = tmp_path / "project"
    project.mkdir()

    def write(index):
        active_coder_state.write(project, {
            "active_coder": "fallback" if index % 2 else "primary",
            "triggering_evidence": {"writer": index},
            "reset_cycle_marker": str(index),
            "last_probe_time": str(index),
        }, tmp_path / "runtime")

    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(write, range(32)))
    result = active_coder_state.read(project, tmp_path / "runtime")
    writer = result["triggering_evidence"]["writer"]
    assert result == {
        "active_coder": "fallback" if writer % 2 else "primary",
        "triggering_evidence": {"writer": writer},
        "reset_cycle_marker": str(writer),
        "last_probe_time": str(writer),
    }
    assert json.loads(active_coder_state.state_path(project, tmp_path / "runtime").read_text()) == result
