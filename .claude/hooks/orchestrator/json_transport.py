"""UTF-8-safe JSON serialization that preserves every Python string code unit."""

from __future__ import annotations

import json
from typing import Any


def dumps(value: Any, **kwargs: Any) -> str:
    r"""Return strict-UTF-8 JSON, escaping only code units UTF-8 cannot encode.

    ``json.dumps(..., ensure_ascii=False)`` leaves lone surrogates in its result.
    Encoding with ``backslashreplace`` turns those code units into JSON ``\uXXXX``
    escapes. ``json.loads`` therefore recovers the exact original string, while
    valid Unicode remains readable and byte-identical to the previous encoding.
    """

    if "ensure_ascii" in kwargs:
        raise TypeError("json_transport.dumps controls ensure_ascii")
    rendered = json.dumps(value, ensure_ascii=False, **kwargs)
    return rendered.encode("utf-8", "backslashreplace").decode("utf-8")


__all__ = ["dumps"]
