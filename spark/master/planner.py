"""Placement planner: decides how a model maps onto cluster devices.

Pure functions over (model entry, device snapshot) so every rule is unit
testable and the policy can evolve without touching the deployer.

Modes:
  - single: the model fits the best single node; zero network overhead.
  - split:  driver node runs llama-server, others run rpc-server and hold
            their share of weights/KV split by llama.cpp.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable, Sequence

from spark.protocol import (
    CTX_OVERHEAD_GB,
    SINGLE_FIT_FACTOR,
    SPLIT_FIT_FACTOR,
)


class PlanError(Exception):
    """Raised when the cluster cannot host the model at all."""


@dataclass
class PlacementPlan:
    mode: str                      # "single" | "split"
    driver_device_id: str
    worker_device_ids: list[str] = field(default_factory=list)
    reasons: list[str] = field(default_factory=list)

    @property
    def node_ids(self) -> list[str]:
        return [self.driver_device_id] + self.worker_device_ids


@dataclass
class Candidate:
    device_id: str
    host: str
    mem_available_gb: float
    cpu_cores: int
    has_gpu: bool


def _candidates(devices: Iterable[dict]) -> list[Candidate]:
    out = []
    for d in devices:
        inv = d.get("inventory", {}) or {}
        out.append(
            Candidate(
                device_id=d["device_id"],
                host=(d.get("control_addr") or "127.0.0.1:0").rsplit(":", 1)[0],
                mem_available_gb=float(inv.get("mem_available", 0)) / 1024**3,
                cpu_cores=int(inv.get("cpu_cores", 0)),
                has_gpu=bool(inv.get("gpus")),
            )
        )
    return out


def _score(c: Candidate) -> tuple:
    # prefer GPU nodes, then free memory, then cores
    return (1 if c.has_gpu else 0, c.mem_available_gb, c.cpu_cores)


def plan_placement(
    approx_gb: float,
    devices: Sequence[dict],
    model_name: str = "",
    busy_device_ids: Sequence[str] = (),
) -> PlacementPlan:
    """Choose single vs split placement for a model over a device snapshot."""
    free = [c for c in _candidates(devices) if c.device_id not in set(busy_device_ids)]
    if not free:
        raise PlanError("no available devices online (all busy or cluster empty)")

    ranked = sorted(free, key=_score, reverse=True)
    best = ranked[0]
    reasons: list[str] = []

    single_need = approx_gb * SINGLE_FIT_FACTOR + CTX_OVERHEAD_GB
    if best.mem_available_gb >= single_need:
        reasons.append(
            f"best node {best.device_id} has {best.mem_available_gb:.1f}GB free >= "
            f"{single_need:.1f}GB needed for single mode"
        )
        return PlacementPlan(mode="single", driver_device_id=best.device_id, reasons=reasons)

    split_need = approx_gb * SPLIT_FIT_FACTOR
    pool = sorted(free, key=lambda c: c.mem_available_gb, reverse=True)
    chosen: list[Candidate] = []
    total = 0.0
    for c in pool:
        chosen.append(c)
        total += c.mem_available_gb
        reasons.append(
            f"added {c.device_id} ({c.mem_available_gb:.1f}GB free, cumulative {total:.1f}GB)"
        )
        if total >= split_need:
            break
    if total < split_need:
        label = model_name or f"{approx_gb:.1f}GB"
        raise PlanError(
            f"cannot host {label}: cluster offers "
            f"{total:.1f}GB free across {len(chosen)} node(s), need {split_need:.1f}GB"
        )
    # the largest-memory node drives; the rest serve as rpc workers
    driver = chosen[0]
    workers = [c.device_id for c in chosen[1:]]
    reasons.append(f"split mode: driver {driver.device_id} + {len(workers)} rpc worker(s)")
    return PlacementPlan(
        mode="split", driver_device_id=driver.device_id, worker_device_ids=workers, reasons=reasons
    )
