"""Deployment state persistence and master-restart adoption tests."""

import json
import time

from spark.master.deployer import Deployer, Deployment
from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry


def make_deployer(tmp_path, monkeypatch, health=True):
    state = str(tmp_path / "deployments.json")
    reg = DeviceRegistry(ttl=60)
    reg.upsert({
        "device_id": "node-a", "name": "node-a", "control_addr": "node-a:3971",
        "inventory": {"mem_available": 4 * 1024**3, "cpu_cores": 8, "gpus": [], "macs": []},
        "workers": [], "ts": time.time(),
    })
    dep = Deployer(reg, ModelRegistry.load(), state_path=state)
    monkeypatch.setattr(dep, "_provision",
                        lambda d, e, p: (setattr(d, "status", "active"),
                                         setattr(d, "driver_base_url", "http://node-a:8300")))
    monkeypatch.setattr(dep, "_probe_health", lambda url: health)
    dep.deploy("qwen2.5-0.5b-instruct")
    time.sleep(0.2)
    return dep, state


def test_state_written_on_deploy(tmp_path, monkeypatch):
    dep, state = make_deployer(tmp_path, monkeypatch)
    data = json.loads(open(state, encoding="utf-8").read())
    assert data["deployments"][0]["model"] == "qwen2.5-0.5b-instruct"
    assert data["deployments"][0]["status"] in ("provisioning", "active")


def test_restart_adopts_healthy_backend(tmp_path, monkeypatch):
    dep, state = make_deployer(tmp_path, monkeypatch, health=True)
    dep.persist()

    reg2 = DeviceRegistry(ttl=60)
    dep2 = Deployer(reg2, ModelRegistry.load(), state_path=state)
    monkeypatch.setattr(dep2, "_probe_health", lambda url: True)
    dep2.reconcile()

    snap = {d["model"]: d for d in dep2.deployments()}["qwen2.5-0.5b-instruct"]
    assert snap["status"] == "active"
    assert any("adopted after master restart" in r for r in snap["reasons"])


def test_restart_flags_dead_backend(tmp_path, monkeypatch):
    dep, state = make_deployer(tmp_path, monkeypatch, health=True)
    dep.persist()

    dep2 = Deployer(DeviceRegistry(ttl=60), ModelRegistry.load(), state_path=state)
    monkeypatch.setattr(dep2, "_probe_health", lambda url: False)
    dep2.reconcile()

    snap = {d["model"]: d for d in dep2.deployments()}["qwen2.5-0.5b-instruct"]
    assert snap["status"] == "failed"
    assert "redeploy" in snap["error"]


def test_no_state_path_writes_nothing(tmp_path, monkeypatch):
    reg = DeviceRegistry(ttl=60)
    dep = Deployer(reg, ModelRegistry.load(), state_path=None)
    dep._persist()  # must be a no-op, not an error
    assert not list(tmp_path.iterdir())


def test_corrupt_state_file_is_ignored(tmp_path):
    state = tmp_path / "deployments.json"
    state.write_text("{not json", encoding="utf-8")
    dep = Deployer(DeviceRegistry(ttl=60), ModelRegistry.load(), state_path=str(state))
    assert dep.deployments() == []
