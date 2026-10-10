from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import policy


@pytest.mark.parametrize("model", [
    "gpt-6-sol", "openai/custom-model", "gemini-3.8-flash-high",
    "gemini-3.8-flash-low",
    "google/gemini-2.5-pro", "deepseek-r1", "claude-sonnet-4-6",
    "qwen3-32b", "unknown/model", "o3xyz-model",
])
def test_model_family_recognizes_known_vendors(model):
    # Go policy validates route syntax but has no general model-ID family classifier.
    assert policy.model_family(model) == {
        "gpt-6-sol": "gpt",
        "openai/custom-model": "openai",
        "gemini-3.8-flash-high": "gemini",
        "gemini-3.8-flash-low": "gemini",
        "google/gemini-2.5-pro": "google",
        "deepseek-r1": "deepseek",
        "claude-sonnet-4-6": "claude",
        "qwen3-32b": "qwen",
        "unknown/model": None,
        "o3xyz-model": None,
    }[model]


def test_vendor_family_accepts_none():
    # Go's vendor-family behavior is string-only, so it has no nil-input counterpart.
    assert policy.vendor_family(None) is None
