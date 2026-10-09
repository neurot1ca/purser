import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_receipts.json"
sys.path.insert(0, str(ORCH))
import compaction_advisor as advisor


def _oracle(monkeypatch, tmp_path):
    monkeypatch.setenv("PURSER_STATE_DIR", str(ROOT / ".tmp" / "post-receipts-state"))
    cases = []
    for name in ("queue", "delivered", "guidance", "absent", "corrupt"):
        repo = tmp_path / name
        repo.mkdir()
        event = {"session_id": "post-receipts-session"}
        path = advisor._state_directory(repo) / "compaction_advisor.json"
        state = advisor._read_state(path, event["session_id"])
        bundle = None
        if name == "queue":
            first, second = "> ⚓ **Codex: first**", "> ⚓ **Codex: second**"
            bundle = {"model": "gpt-6-sol", "effort": "high"}
            advisor.queue_receipt(event, repo, bundle)
            advisor.queue_dispatch(event, repo, line=first)
            advisor.queue_dispatch(event, repo, line=second)
            state = advisor._read_state(path, event["session_id"])
            expected = {"lines": state["dispatch_pending"]["lines"], "receipt": state["receipt_pending"],
                        "guidance": advisor.pending_guidance(event, repo)}
        elif name == "delivered":
            state["version_pending"] = {"version": "v1", "delivered": False}
            advisor._atomic_write(path, json.dumps(state, sort_keys=True).encode())
            advisor.mark_lifecycle_guidance_delivered(event, repo)
            once = advisor._read_state(path, event["session_id"])["version_pending"]["delivered"]
            advisor.mark_lifecycle_guidance_delivered(event, repo)
            expected = {"delivered_once": once, "delivered_repeat": advisor._read_state(path, event["session_id"])["version_pending"]["delivered"]}
        elif name == "guidance":
            advisor.queue_receipt(event, repo, {"model": "gpt-6-sol", "effort": "high"})
            advisor.queue_dispatch(event, repo, line="> ⚓ **Codex: finished job**")
            state = advisor._read_state(path, event["session_id"])
            expected = {"guidance": advisor.pending_guidance(event, repo)}
        elif name == "corrupt":
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("{broken", encoding="utf-8")
            expected = {"guidance": advisor.pending_guidance(event, repo)}
        else:
            expected = {"guidance": advisor.pending_guidance(event, repo)}
        cases.append({"name": name, "repository": f"__REPO__/{name}", "initial": state, "bundle": bundle, "expected": expected})
    return {"oracle": "compaction_advisor receipt queue, delivery and pending guidance", "cases": cases}


def test_post_receipts_match_python_fixture(monkeypatch, tmp_path):
    fixture = _oracle(monkeypatch, tmp_path)
    if os.environ.get("POST_RECEIPTS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8", newline="\n")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    import subprocess
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"),
           "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp")}
    result = subprocess.run(
        ["go", "test", "-run", "^TestPostReceiptsPythonFixture$", "-count=1", "./internal/enforce"],
        cwd=ROOT / "go", env=env, capture_output=True, text=True,
    )
    assert result.returncode == 0, result.stdout + result.stderr
