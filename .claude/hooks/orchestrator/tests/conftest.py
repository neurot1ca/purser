from pathlib import Path
import sys


orchestrator_dir = str(Path(__file__).resolve().parents[1])
if orchestrator_dir not in sys.path:
    sys.path.insert(0, orchestrator_dir)
