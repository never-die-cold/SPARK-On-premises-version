"""Idle auto-sleep policy tests (no real network, no real sleep)."""

import time

from spark.master.power import IdlePolicy
from spark.master.registry import DeviceRegistry


def announce(device_id, host, workers=None):
    return {
        "device_id": device_id,
        "name": device_id,
        "control_addr": f"{host}:3971",
        "inventory": {"mem_available": 4 * 1024**3, "cpu_cores": 8, "gpus": [], "macs": []},
        "workers": workers or [],
        "ts": time.time(),
    }


class FakeDeployer:
    def __init__(self):
        self.calls = []

    def deployments(self):
        return []

    def _agent_call(self, control_addr, method, path, json=None, timeout="status"):
        self.calls.append((control_addr, path))
        return {}


class FakeDeployerBusy(FakeDeployer):
    def deployments(self):
        return [{"status": "active", "driver_device_id": "busy-node",
                 "worker_device_ids": []}]


def test_sleeps_idle_remote_node_after_threshold():
    reg = DeviceRegistry(ttl=60)
    reg.upsert(announce("idle-node", "203.0.113.9"))
    dep = FakeDeployer()
    policy = IdlePolicy(dep, reg, minutes=0.0001)  # ~6ms
    policy._local = {"127.0.0.1"}  # remote node not local
    policy.tick()  # first tick starts the idle clock
    time.sleep(0.02)
    policy.tick()
    assert dep.calls and dep.calls[0][1] == "/power/sleep"


def test_never_sleeps_local_machine():
    reg = DeviceRegistry(ttl=60)
    reg.upsert(announce("master-host", "127.0.0.1"))
    dep = FakeDeployer()
    policy = IdlePolicy(dep, reg, minutes=0.0001)
    policy._local = {"127.0.0.1"}
    time.sleep(0.02)
    policy.tick()
    assert dep.calls == []


def test_never_sleeps_busy_or_running_worker():
    reg = DeviceRegistry(ttl=60)
    reg.upsert(announce("busy-node", "203.0.113.10"))
    reg.upsert(announce("working-node", "203.0.113.11",
                        workers=[{"id": "server-8300", "state": "running"}]))
    dep = FakeDeployerBusy()
    policy = IdlePolicy(dep, reg, minutes=0.0001)
    policy._local = {"127.0.0.1"}
    time.sleep(0.02)
    policy.tick()
    assert dep.calls == []


def test_backoff_after_sleep_command():
    reg = DeviceRegistry(ttl=60)
    reg.upsert(announce("idle-node", "203.0.113.12"))
    dep = FakeDeployer()
    policy = IdlePolicy(dep, reg, minutes=0.0001)
    policy._local = {"127.0.0.1"}
    policy.tick()  # start idle clock
    time.sleep(0.02)
    policy.tick()  # goes to sleep
    policy.tick()  # immediately again -> must not re-send
    assert len(dep.calls) == 1
