import pytest

import test_distribution


@pytest.mark.parametrize(
    ("machine", "arch"),
    [
        ("x86_64", "amd64"),
        ("amd64", "amd64"),
        ("AMD64", "amd64"),
        ("aarch64", "arm64"),
        ("arm64", "arm64"),
        ("ARM64", "arm64"),
    ],
)
def test_host_target_normalizes_machine_case(monkeypatch, machine, arch):
    monkeypatch.setattr(test_distribution.platform, "system", lambda: "Linux")
    monkeypatch.setattr(test_distribution.platform, "machine", lambda: machine)

    assert test_distribution.host_target() == ("linux", arch, f"purser-hook-linux-{arch}")
