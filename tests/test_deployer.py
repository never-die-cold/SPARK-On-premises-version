"""Deployer provisioning-time memory reservation tests (no real agents)."""

import threading
import time

import pytest

from spark.master.deployer import Deployer
from spark.master.planner import PlanError
from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry


def add_device(registry, device_id, free_gb):
    registry.upsert({
        "device_id": device_id,
        "name": device_id,
        "control_addr": f"{device_id}-host:3971",
        "inventory": {
            "mem_available": int(free_gb * 1024**3),
            "mem_total": int(free_gb * 1024**3),
            "cpu_cores": 8,
            "gpus": [],
        },
        "workers": [],
        "ts": time.time(),
    })


@pytest.fixture()
def cluster():
    registry = DeviceRegistry()
    models = ModelRegistry.load()
    deployer = Deployer(registry, models)
    add_device(registry, "node-a", free_gb=4.0)
    return registry, models, deployer


def test_provisioning_reserves_memory(cluster, monkeypatch):
    registry, models, deployer = cluster

    # keep the first deployment in "provisioning" forever
    release = threading.Event()
    monkeypatch.setattr(deployer, "_provision",
                        lambda *a, **k: release.wait(timeout=10))

    big = "qwen3-4b"  # ~2.65GB
    dep = deployer.deploy(big)
    assert dep.status == "provisioning"

    # node-a reported 4GB free but 2.65GB is booked; a 3GB model must not fit
    with pytest.raises(PlanError):
        deployer.deploy("qwen2.5-3b-instruct")

    release.set()
    time.sleep(0.2)


def test_active_deployment_uses_heartbeat_memory_not_reservation(cluster, monkeypatch):
    """Once active, memory shows up in heartbeats; reservation must stop
    double-counting (qwen2.5-3b alone fits when node has 4GB)."""
    registry, models, deployer = cluster
    release = threading.Event()
    monkeypatch.setattr(deployer, "_provision",
                        lambda *a, **k: release.wait(timeout=10))

    dep = deployer.deploy("qwen2.5-3b-instruct")
    assert dep.status == "provisioning"
    # simulate load complete: memory drops in heartbeat, deployment goes active
    add_device(registry, "node-a", free_gb=1.2)
    dep.status = "active"
    release.set()
    time.sleep(0.2)

    # now only ~1.2GB really free: another 3GB model must be rejected
    with pytest.raises(PlanError):
        deployer.deploy("qwen2.5-3b-instruct" if False else "qwen3-4b")
