import json

import model_catalog


def _payload():
    return json.dumps({
        "openai": {
            "name": "ignored provider metadata",
            "models": {
                "sample-model": {
                    "name": "Sample Model",
                    "family": "sample-family",
                    "reasoning": True,
                    "cost": {"input": 0, "output": 0, "cache_read": 99},
                    "limit": {"context": 12000, "output": 2000},
                    "unknown": "ignored",
                },
                "bad-model": {
                    "name": "Bad Model",
                    "reasoning": False,
                    "limit": {"context": "unknown"},
                },
            },
        }
    }).encode()


class _Response:
    def __init__(self, payload):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return None

    def read(self, size):
        return self.payload[:size]


def test_good_fetch_writes_checked_cache_and_ignores_unknown_fields(tmp_path, monkeypatch):
    requests = []

    def fetch(request, **_kwargs):
        requests.append(request)
        return _Response(_payload())

    monkeypatch.setattr(model_catalog, "urlopen", fetch)

    result = model_catalog.load_catalog(root=tmp_path)

    assert result["source"] == "network"
    assert requests[0].get_header("User-agent") == "purser"
    assert result["fetched_at"]
    assert result["dropped_count"] == 1
    assert result["providers"]["openai"]["sample-model"] == {
        "id": "sample-model",
        "name": "Sample Model",
        "family": "sample-family",
        "reasoning": True,
        "cost": {"input": 0, "output": 0},
        "limits": {"context": 12000, "output": 2000},
    }
    cached = json.loads((tmp_path / "model-catalog" / "catalog.json").read_text(encoding="utf-8"))
    assert cached["dropped_count"] == 1
    assert cached["providers"]["openai"]["models"] == result["providers"]["openai"]


def test_huge_integer_cost_is_ignored_without_rejecting_model():
    catalog = {
        "openai": {"models": {
            "huge-cost": {
                "name": "Huge Cost",
                "reasoning": False,
                "cost": {"input": 10**400, "output": 1},
            },
        }},
    }

    providers, dropped_count = model_catalog._read_document(json.dumps(catalog).encode())

    assert providers["openai"]["huge-cost"] == {
        "id": "huge-cost", "name": "Huge Cost", "reasoning": False, "limits": {},
    }
    assert dropped_count == 0


def _assert_last_good(tmp_path, monkeypatch, configure_failure):
    monkeypatch.setattr(model_catalog, "urlopen", lambda *_args, **_kwargs: _Response(_payload()))
    good = model_catalog.load_catalog(root=tmp_path)
    cache_path = tmp_path / "model-catalog" / "catalog.json"
    original_cache = cache_path.read_bytes()
    configure_failure(monkeypatch)
    result = model_catalog.load_catalog(root=tmp_path)

    assert result["source"] == "cache"
    assert result["fetched_at"] == good["fetched_at"]
    assert result["providers"] == good["providers"]
    assert result["dropped_count"] == good["dropped_count"]
    assert cache_path.read_bytes() == original_cache


def test_malformed_fetch_keeps_last_good_cache(tmp_path, monkeypatch):
    _assert_last_good(
        tmp_path,
        monkeypatch,
        lambda monkeypatch: monkeypatch.setattr(
            model_catalog,
            "urlopen",
            lambda *_args, **_kwargs: _Response(
                b'{"openai":{"models":{"partial":{"name":"Missing reasoning"}}}}'
            ),
        ),
    )


def test_oversized_fetch_keeps_last_good_cache(tmp_path, monkeypatch):
    def configure(monkeypatch):
        too_large = b"x" * (model_catalog.MAX_RESPONSE_BYTES + 1)
        monkeypatch.setattr(model_catalog, "urlopen", lambda *_args, **_kwargs: _Response(too_large))

    _assert_last_good(tmp_path, monkeypatch, configure)


def test_timeout_keeps_last_good_cache(tmp_path, monkeypatch):
    def configure(monkeypatch):
        def timeout(*_args, **_kwargs):
            raise TimeoutError("models.dev timed out")

        monkeypatch.setattr(model_catalog, "urlopen", timeout)

    _assert_last_good(tmp_path, monkeypatch, configure)


def test_no_cache_and_fetch_failure_returns_bootstrap(tmp_path, monkeypatch):
    def timeout(*_args, **_kwargs):
        raise TimeoutError("models.dev timed out")

    monkeypatch.setattr(model_catalog, "urlopen", timeout)

    result = model_catalog.load_catalog(root=tmp_path)

    assert result["source"] == "bootstrap"
    assert result["fetched_at"] is None
    assert {"openai", "google", "anthropic", "opencode"} <= set(result["providers"])
    assert not (tmp_path / "model-catalog" / "catalog.json").exists()
