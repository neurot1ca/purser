import ast
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SUBPROCESS_METHODS = {"run", "Popen", "check_output", "call"}


def test_subprocess_launches_declare_creationflags():
    missing = []
    for path in ROOT.rglob("*.py"):
        if "tests" in path.relative_to(ROOT).parts or "__pycache__" in path.parts:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if not (
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Attribute)
                and isinstance(node.func.value, ast.Name)
                and node.func.value.id == "subprocess"
                and node.func.attr in SUBPROCESS_METHODS
            ):
                continue
            if not any(keyword.arg == "creationflags" for keyword in node.keywords):
                missing.append(f"{path.relative_to(ROOT)}:{node.lineno}")

    assert not missing, "subprocess launches missing creationflags: " + ", ".join(missing)
