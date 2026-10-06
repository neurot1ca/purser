"""Shared real-process fixtures for Go/Python durable-job interoperability."""
import os
from pathlib import Path
import subprocess

import pytest


@pytest.fixture(scope="session")
def go_test_tools():
    go_root = Path(__file__).parents[4] / "go"
    scratch = go_root / ".tmp"
    scratch.mkdir(exist_ok=True)
    cache = scratch / "gocache"
    hook = scratch / ("purser-hook-mixed.exe" if os.name == "nt" else "purser-hook-mixed")
    fake = scratch / ("fake-codex.exe" if os.name == "nt" else "fake-codex")
    source = scratch / "fake-codex.go"
    source.write_text('''package main
import (
 "encoding/json"
 "os"
 "time"
)
func emit(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
func main() {
 args := os.Args[1:]
 if marker := os.Getenv("MIXED_LAUNCH_MARKER"); marker != "" { f, _ := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); if f != nil { _, _ = f.WriteString("launch\\n"); _ = f.Close() } }
 emit(map[string]any{"type":"thread.started", "thread_id":"mixed-thread"})
 emit(map[string]any{"type":"turn.started"})
 if os.Getenv("FAKE_HANG_BEFORE_TERMINAL") == "1" { for { time.Sleep(time.Second) } }
 emit(map[string]any{"type":"item.completed", "item":map[string]any{"type":"agent_message", "text":"mixed result"}})
 emit(map[string]any{"type":"turn.completed", "usage":map[string]any{}})
 for i, arg := range args { if arg == "-o" && i+1 < len(args) { _ = os.WriteFile(args[i+1], []byte("mixed result"), 0600); return } }
}
''', encoding="utf-8")
    env = {**os.environ, "GOCACHE": str(cache)}
    subprocess.run(["go", "build", "-o", str(hook), "./cmd/purser-hook"], cwd=go_root,
                   env=env, check=True, timeout=120)
    subprocess.run(["go", "build", "-o", str(fake), str(source)], cwd=go_root,
                   env=env, check=True, timeout=120)
    return {"hook": hook, "fake": fake}


def launch_go_host(hook, record_path, env):
    """Run a Go-created queued generation directly when detached launch is unavailable in CI."""
    import json

    record = json.loads(record_path.read_text(encoding="utf-8"))
    if record.get("state") != "QUEUED":
        return None
    command = [str(hook), "host", str(record_path.parent), record["jobId"],
               str(record["generation"]), record["ownershipToken"]]
    flags = getattr(subprocess, "CREATE_NO_WINDOW", 0) if os.name == "nt" else 0
    return subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, env=env, creationflags=flags)
