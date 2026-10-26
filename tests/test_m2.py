"""M2 tests: WoL packets, heartbeat token auth, auto-heal re-planning."""

import time

import pytest
from fastapi.testclient import TestClient

from spark.master import wol
from spark.master.app import create_app
from spark.master.deployer import Deployer, server_extra_args
from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry


class TestMagicPacket:
    def test_packet_structure(self):
        p = wol.magic_packet("AA:BB:CC:DD:EE:FF")
        assert len(p) == 102  # 6 sync + 16*6 mac
        assert p[:6] == b"\xff" * 6
        assert p[6:12] == bytes.fromhex("AABBCCDDEEFF")
        assert p[-6:] == bytes.fromhex("AABBCCDDEEFF")

    def test_accepts_separators_and_case(self):
        assert wol.magic_packet("aa-bb-cc-dd-ee-ff") == wol.magic_packet("AA:BB:CC:DD:EE:FF")

    def test_rejects_bad_mac(self):
        with pytest.raises(ValueError):
            wol.magic_packet("not-a-mac")


class TestHeartbeatToken:
    def _client(self, token):
        app = create_app(start_discovery=False, token=token)
        return TestClient(app)

    def test_no_token_by_default(self):
        client = self._client("")
        r = client.post("/api/heartbeat", json={"device_id": "d1", "control_addr": "h:1"})
        assert r.status_code == 200

    def test_token_required_when_configured(self):
        client = self._client("s3cret")
        body = {"device_id": "d1", "control_addr": "h:1"}
        assert client.post("/api/heartbeat", json=body).status_code == 401
        assert client.post("/api/heartbeat", json=body,
                           headers={"X-SPARK-Token": "wrong"}).status_code == 401
        assert client.post("/api/heartbeat", json=body,
                           headers={"X-SPARK-Token": "s3cret"}).status_code == 200


def announce(device_id, free_gb):
    return {
        "device_id": device_id,
        "name": device_id,
        "control_addr": f"{device_id}-host:3971",
        "inventory": {"mem_available": int(free_gb * 1024**3),
                       "mem_total": int(free_gb * 1024**3),
                       "cpu_cores": 8, "gpus": [], "macs": ["aa:bb:cc:00:00:01"]},
        "workers": [],
        "ts": int(time.time()),
    }


class TestServerArgs:
    def test_gpu_driver_gets_ngl(self):
        from spark.master.registry import Device

        gpu_dev = Device(device_id="d", name="d", control_addr="h:1",
                         inventory={"gpus": [{"name": "RTX", "vram_bytes": 8 << 30}]})
        assert server_extra_args(gpu_dev) == ["-ngl", "99"]

    def test_cpu_driver_gets_no_flags(self):
        from spark.master.registry import Device

        cpu_dev = Device(device_id="d", name="d", control_addr="h:1",
                         inventory={"gpus": []})
        assert server_extra_args(cpu_dev) == []
        assert server_extra_args(None) == []

    def test_ngl_env_override(self, monkeypatch):
        from spark.master.registry import Device

        monkeypatch.setenv("SPARK_NGL", "20")
        gpu_dev = Device(device_id="d", name="d", control_addr="h:1",
                         inventory={"gpus": [{"name": "RTX"}]})
        assert server_extra_args(gpu_dev) == ["-ngl", "20"]


class TestBackendHealth:
    def _deployer_with_active(self, monkeypatch, health_ok):
        registry = DeviceRegistry(ttl=5.0)
        deployer = Deployer(registry, ModelRegistry.load())
        monkeypatch.setattr(deployer, "HEAL_GRACE_SECONDS", 0.05)
        monkeypatch.setattr(deployer, "_cleanup_partial", lambda dep: None)
        monkeypatch.setattr(deployer, "_probe_health", lambda url: health_ok)
        registry.upsert(announce("node-a", 4.0))

        def fake_provision(dep, entry, plan):
            dep.driver_base_url = "http://node-a:8300"
            dep.status = "active"
        monkeypatch.setattr(deployer, "_provision", fake_provision)
        deployer.deploy("qwen2.5-3b-instruct")
        return deployer

    def test_health_failures_degrade_and_trigger_heal(self, monkeypatch):
        deployer = self._deployer_with_active(monkeypatch, health_ok=True)
        monkeypatch.setattr(deployer, "_probe_health", lambda url: False)

        for _ in range(2):  # below threshold
            deployer.check_backends()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "active"

        deployer.check_backends()  # 3rd consecutive failure crosses threshold
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "degraded"
        assert snap["degraded_cause"] == "backend_dead"

        time.sleep(0.3)  # heal (grace shrunk) redeploys
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["heal_attempts"] == 1
        assert snap["status"] in ("active", "provisioning")

    def test_healthy_probe_resets_counter(self, monkeypatch):
        deployer = self._deployer_with_active(monkeypatch, health_ok=True)
        checks = iter([False, False, True, False, False])
        monkeypatch.setattr(deployer, "_probe_health", lambda url: next(checks))
        for _ in range(5):
            deployer.check_backends()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "active"  # never 3 consecutive failures

    def test_backend_dead_is_not_recovered_by_node_watch(self, monkeypatch):
        deployer = self._deployer_with_active(monkeypatch, health_ok=True)
        monkeypatch.setattr(deployer, "_probe_health", lambda url: False)
        for _ in range(3):
            deployer.check_backends()
        # nodes are all online; the node-level recovery branch must NOT
        # flip a backend_dead deployment back to active
        deployer.check_and_heal()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["degraded_cause"] == "backend_dead"


class TestAutoHeal:
    def test_degraded_deployment_heals_onto_surviving_node(self, monkeypatch):
        registry = DeviceRegistry(ttl=0.3)
        models = ModelRegistry.load()
        deployer = Deployer(registry, models)
        monkeypatch.setattr(deployer, "HEAL_GRACE_SECONDS", 0.05)
        # worker cleanup talks to (fake) agents and can stall on DNS; it is
        # orthogonal to the heal decision under test
        monkeypatch.setattr(deployer, "_cleanup_partial", lambda dep: None)
        registry.upsert(announce("node-a", 4.0))
        registry.upsert(announce("node-b", 4.0))

        # provisioning that flips to active immediately
        def fake_provision(dep, entry, plan):
            dep.status = "active"
            dep.driver_base_url = "http://x:8300"
        monkeypatch.setattr(deployer, "_provision", fake_provision)

        dep = deployer.deploy("qwen2.5-3b-instruct")
        assert dep.status == "active"

        # node-a dies (stop heartbeats), node-b keeps beating
        registry.upsert(announce("node-b", 4.0))
        time.sleep(0.4)  # let node-a TTL out

        deployer.check_and_heal()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "degraded"

        # keep node-b beating while the heal thread (short grace) re-plans;
        # its TTL (0.3s) would otherwise expire during the wait
        deadline = time.time() + 0.5
        while time.time() < deadline:
            registry.upsert(announce("node-b", 4.0))
            time.sleep(0.05)

        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] in ("active", "provisioning"), snap
        assert snap["heal_attempts"] == 1
        assert any("auto-heal" in r for r in snap["reasons"])
        assert snap["driver_device_id"] == "node-b"  # survived node takes over

    def test_degraded_recovers_when_node_returns(self, monkeypatch):
        registry = DeviceRegistry(ttl=0.3)
        models = ModelRegistry.load()
        deployer = Deployer(registry, models)
        monkeypatch.setattr(deployer, "HEAL_GRACE_SECONDS", 5.0)  # heal must NOT fire
        registry.upsert(announce("node-a", 4.0))

        def fake_provision(dep, entry, plan):
            dep.status = "active"
        monkeypatch.setattr(deployer, "_provision", fake_provision)
        deployer.deploy("qwen2.5-3b-instruct")

        time.sleep(0.4)  # node-a TTLs out
        deployer.check_and_heal()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "degraded"

        registry.upsert(announce("node-a", 4.0))  # node comes back
        deployer.check_and_heal()
        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "active"
        assert any("live again" in r for r in snap["reasons"])

    def test_heal_gives_up_after_max_attempts(self, monkeypatch):
        registry = DeviceRegistry(ttl=0.2)
        models = ModelRegistry.load()
        deployer = Deployer(registry, models)
        monkeypatch.setattr(deployer, "HEAL_GRACE_SECONDS", 0.02)

        registry.upsert(announce("only-node", 4.0))
        monkeypatch.setattr(deployer, "_cleanup_partial", lambda dep: None)
        monkeypatch.setattr(deployer, "_provision",
                            lambda dep, entry, plan: setattr(dep, "status", "active"))
        dep = deployer.deploy("qwen2.5-3b-instruct")
        assert dep.status == "active"

        time.sleep(0.3)  # node TTLs out -> no nodes at all
        for _ in range(4):
            deployer.check_and_heal()
            time.sleep(0.15)

        snap = {d["model"]: d for d in deployer.deployments()}["qwen2.5-3b-instruct"]
        assert snap["status"] == "failed"
        assert "auto-heal" in snap["error"]
