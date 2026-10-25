"""Registry TTL and model registry tests."""

import time

from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry


def announce(device_id, free_gb=4, name=None, control=None):
    return {
        "device_id": device_id,
        "name": name or device_id,
        "control_addr": control or f"{device_id}-host:3971",
        "inventory": {"mem_available": int(free_gb * 1024**3), "mem_total": int(free_gb * 1024**3),
                       "cpu_cores": 8, "gpus": []},
        "workers": [],
        "ts": int(time.time()),
    }


class TestRegistry:
    def test_upsert_and_snapshot(self):
        r = DeviceRegistry()
        r.upsert(announce("d1", free_gb=4))
        r.upsert(announce("d1", free_gb=2))  # refresh + update
        snap = r.snapshot()
        assert len(snap) == 1
        assert snap[0]["online"] is True
        assert snap[0]["inventory"]["mem_available"] == int(2 * 1024**3)

    def test_ttl_expiry_marks_offline(self):
        r = DeviceRegistry(ttl=0.05)
        r.upsert(announce("d1"))
        time.sleep(0.1)
        snap = r.snapshot()
        assert snap[0]["online"] is False
        assert r.is_online("d1") is False
        assert r.online_devices() == []

    def test_host_extraction(self):
        r = DeviceRegistry()
        d = r.upsert(announce("d1", control="192.168.1.9:3971"))
        assert d.host == "192.168.1.9"


class TestModelRegistry:
    def test_load_builtin_entries(self):
        reg = ModelRegistry.load()
        assert len(reg) >= 5
        for name in reg.names():
            entry = reg.get(name)
            assert entry.url.startswith("https://")
            assert entry.filename.endswith(".gguf")
            assert entry.approx_gb > 0
            assert entry.approx_bytes == int(entry.approx_gb * 1024**3)

    def test_unknown_model_is_none(self):
        reg = ModelRegistry.load()
        assert reg.get("gpt-4") is None
