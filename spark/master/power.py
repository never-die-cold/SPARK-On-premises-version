"""Idle auto-sleep policy (opt-in via SPARK_IDLE_SLEEP_MIN).

Puts devices that have no workers and are not part of any live deployment to
sleep after N idle minutes, so an idle cluster stops burning power. Safety
rails: never sleeps the master's own machine, never sleeps a node with
running/restarting workers or belonging to a non-stopped deployment.
Wake-up is manual (`spark wake`) or triggered by a future deploy request
(M2+); a sleeping device simply drops out of the registry.
"""

from __future__ import annotations

import logging
import socket
import time

from spark.master.deployer import Deployer
from spark.master.registry import DeviceRegistry

log = logging.getLogger(__name__)

LIVE_STATUSES = ("planning", "provisioning", "active", "degraded", "stopping")


def _local_hosts() -> set[str]:
    hosts = {"127.0.0.1", "::1", "localhost"}
    try:
        _, _, ips = socket.gethostbyname_ex(socket.gethostname())
        hosts.update(ips)
    except OSError:
        pass
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("223.5.5.5", 53))
        hosts.add(s.getsockname()[0])
        s.close()
    except OSError:
        pass
    return hosts


class IdlePolicy:
    def __init__(self, deployer: Deployer, registry: DeviceRegistry, minutes: float):
        self.deployer = deployer
        self.registry = registry
        self.idle_seconds = minutes * 60.0
        self._since: dict[str, float] = {}
        self._local = _local_hosts()

    def _busy_device_ids(self) -> set[str]:
        busy: set[str] = set()
        for dep in self.deployer.deployments():
            if dep["status"] in LIVE_STATUSES:
                busy.add(dep["driver_device_id"])
                busy.update(dep["worker_device_ids"])
        return busy

    def tick(self) -> None:
        now = time.time()
        busy = self._busy_device_ids()
        for snap in self.registry.snapshot():
            did = snap["device_id"]
            dev = self.registry.get(did)
            if dev is None:
                continue
            if not snap["online"]:
                self._since.pop(did, None)
                continue
            running = any(w.get("state") in ("running", "restarting")
                          for w in snap.get("workers", []))
            if did in busy or running or dev.host in self._local:
                self._since[did] = now
                continue
            since = self._since.setdefault(did, now)
            if now - since < self.idle_seconds:
                continue
            try:
                self.deployer._agent_call(dev.control_addr, "POST", "/power/sleep",
                                          timeout="status")
                log.info("idle policy: put %s to sleep after %.0f min idle",
                         did, (now - since) / 60)
            except RuntimeError as exc:
                log.info("idle policy: %s refused sleep (%s)", did, exc)
            self._since[did] = now  # back off regardless of outcome
