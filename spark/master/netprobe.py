"""Master-side network probing between cluster nodes.

Measures master<->agent throughput and RTT via the agent's /netprobe
endpoint. In a home LAN the master sits on the same fabric as the nodes, so
these numbers classify each node's link (wired vs Wi-Fi vs poor) well enough
to gate split-mode placements. Node-to-node bandwidth between two workers is
approximated by the two master-side measurements (see docs/architecture.md).
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass
from typing import Optional

import httpx

log = logging.getLogger(__name__)

DEFAULT_PROBE_BYTES = 2 * 1024 * 1024
PROBE_TIMEOUT = 6.0


@dataclass
class NetStat:
    device_id: str
    mbps: float = 0.0
    rtt_ms: float = 0.0
    ok: bool = False

    @property
    def link_class(self) -> str:
        """Coarse link bucket used by the planner (thresholds are engineering
        defaults, not calibrated by benchmarks)."""
        if not self.ok:
            return "unknown"
        if self.mbps >= 300 and self.rtt_ms <= 10:
            return "wired"
        if self.mbps >= 80:
            return "wifi"
        if self.mbps >= 30:
            return "slow"
        return "poor"

    def to_dict(self) -> dict:
        return {
            "device_id": self.device_id,
            "mbps": round(self.mbps, 1),
            "rtt_ms": round(self.rtt_ms, 2),
            "ok": self.ok,
            "link_class": self.link_class,
        }


def probe_node(control_addr: str, token: str = "", bytes: int = DEFAULT_PROBE_BYTES,
               timeout: float = PROBE_TIMEOUT,
               client: Optional[httpx.Client] = None) -> tuple[float, float]:
    """Return (mbps, rtt_ms) for one agent; raises httpx.HTTPError on failure.
    Pass a pooled client when probing several nodes to reuse connections."""
    headers = {"X-SPARK-Token": token} if token else {}
    url = f"http://{control_addr}/netprobe"
    own = client is None
    if own:
        client = httpx.Client(trust_env=False)
    try:
        t0 = time.perf_counter()
        received = 0
        with client.stream("GET", url, params={"bytes": bytes}, timeout=timeout,
                           headers=headers) as resp:
            resp.raise_for_status()
            ttfb = time.perf_counter() - t0
            for chunk in resp.iter_bytes():
                received += len(chunk)
    finally:
        if own:
            client.close()
    elapsed = time.perf_counter() - t0 - ttfb
    if elapsed <= 0 or received <= 0:
        raise httpx.HTTPError(f"empty probe response from {control_addr}")
    mbps = received * 8 / elapsed / 1e6
    return mbps, ttfb * 1000.0


def probe_all(devices: list, token: str = "") -> dict[str, NetStat]:
    """Probe every given registry Device; failures degrade to ok=False."""
    out: dict[str, NetStat] = {}
    client = httpx.Client(trust_env=False)
    try:
        for dev in devices:
            stat = NetStat(device_id=dev.device_id)
            try:
                mbps, rtt = probe_node(dev.control_addr, token, client=client)
                stat.mbps, stat.rtt_ms, stat.ok = mbps, rtt, True
            except (httpx.HTTPError, OSError) as exc:
                log.debug("netprobe %s failed: %s", dev.device_id, exc)
            out[dev.device_id] = stat
    finally:
        client.close()
    return out
