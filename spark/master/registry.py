"""Cluster device registry with TTL-based liveness."""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any, Optional

DEFAULT_TTL_SECONDS = 15.0


@dataclass
class Device:
    device_id: str
    name: str
    control_addr: str
    inventory: dict[str, Any] = field(default_factory=dict)
    workers: list[dict[str, Any]] = field(default_factory=list)
    last_seen: float = 0.0
    first_seen: float = 0.0

    @property
    def host(self) -> str:
        return self.control_addr.rsplit(":", 1)[0]

    def mem_available_gb(self) -> float:
        return float(self.inventory.get("mem_available", 0)) / 1024**3

    def mem_total_gb(self) -> float:
        return float(self.inventory.get("mem_total", 0)) / 1024**3

    def has_gpu(self) -> bool:
        return bool(self.inventory.get("gpus"))

    def cpu_cores(self) -> int:
        return int(self.inventory.get("cpu_cores", 0))


class DeviceRegistry:
    """Thread-safe registry of agent announces (UDP or HTTP heartbeat)."""

    def __init__(self, ttl: float = DEFAULT_TTL_SECONDS):
        self.ttl = ttl
        self._lock = threading.RLock()
        self._devices: dict[str, Device] = {}

    def upsert(self, announce: dict[str, Any]) -> Device:
        device_id = announce.get("device_id")
        if not device_id:
            raise ValueError("announce missing device_id")
        now = time.time()
        with self._lock:
            dev = self._devices.get(device_id)
            if dev is None:
                dev = Device(
                    device_id=device_id,
                    name=announce.get("name", device_id),
                    control_addr=announce.get("control_addr", ""),
                    first_seen=now,
                )
                self._devices[device_id] = dev
            dev.name = announce.get("name", dev.name)
            if announce.get("control_addr"):
                dev.control_addr = announce["control_addr"]
            if "inventory" in announce:
                dev.inventory = announce["inventory"] or {}
            if "workers" in announce:
                dev.workers = announce["workers"] or []
            dev.last_seen = now
            return dev

    def get(self, device_id: str) -> Optional[Device]:
        with self._lock:
            return self._devices.get(device_id)

    def is_online(self, device_id: str) -> bool:
        with self._lock:
            dev = self._devices.get(device_id)
            return dev is not None and (time.time() - dev.last_seen) <= self.ttl

    def snapshot(self) -> list[dict[str, Any]]:
        now = time.time()
        with self._lock:
            out = []
            for dev in self._devices.values():
                out.append(
                    {
                        "device_id": dev.device_id,
                        "name": dev.name,
                        "control_addr": dev.control_addr,
                        "online": (now - dev.last_seen) <= self.ttl,
                        "last_seen": dev.last_seen,
                        "first_seen": dev.first_seen,
                        "inventory": dev.inventory,
                        "workers": dev.workers,
                    }
                )
            return out

    def online_devices(self) -> list[Device]:
        now = time.time()
        with self._lock:
            return [d for d in self._devices.values() if now - d.last_seen <= self.ttl]

    def forget(self, device_id: str) -> None:
        with self._lock:
            self._devices.pop(device_id, None)
