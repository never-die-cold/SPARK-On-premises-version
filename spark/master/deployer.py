"""Deployer: drives agents through a placement plan.

Runs deployments on background threads and mutates Deployment records; API
handlers only ever read snapshots. All agent calls go through the control
HTTP surface (httpx, sync -- threads, not asyncio).
"""

from __future__ import annotations

import logging
import os
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Optional

import httpx

from spark.master.planner import PlanError, plan_placement
from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry
from spark.protocol import DEFAULT_RPC_PORT, DEFAULT_SERVER_PORT_BASE

log = logging.getLogger(__name__)

AGENT_TIMEOUTS = {
    "status": httpx.Timeout(5.0),
    "engine": httpx.Timeout(1800.0),   # engine download + extract
    "start": httpx.Timeout(60.0),
    "job": httpx.Timeout(10.0),
    "download": httpx.Timeout(30.0),   # job *start* call only; polled separately
}


def server_extra_args(driver) -> list[str]:
    """Extra llama-server flags for the driver node. Offload all layers when
    a GPU is present (SPARK_NGL overrides); CPU-only builds ignore -ngl."""
    if driver is not None and driver.has_gpu():
        return ["-ngl", os.environ.get("SPARK_NGL", "99")]
    return []


@dataclass
class Deployment:
    model: str
    status: str          # planning|provisioning|active|degraded|failed|stopping|stopped
    mode: str = ""       # single|split
    driver_device_id: str = ""
    worker_device_ids: list[str] = field(default_factory=list)
    driver_base_url: str = ""
    server_port: int = 0
    reasons: list[str] = field(default_factory=list)
    error: str = ""
    heal_attempts: int = 0
    heal_running: bool = field(default=False, repr=False)  # in-flight guard
    degraded_cause: str = ""  # node_offline | backend_dead
    unhealthy_checks: int = 0
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)

    def to_dict(self) -> dict[str, Any]:
        return {
            "model": self.model,
            "status": self.status,
            "mode": self.mode,
            "driver_device_id": self.driver_device_id,
            "worker_device_ids": self.worker_device_ids,
            "driver_base_url": self.driver_base_url,
            "server_port": self.server_port,
            "reasons": self.reasons,
            "error": self.error,
            "heal_attempts": self.heal_attempts,
            "degraded_cause": self.degraded_cause,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
        }

    def touch(self) -> None:
        self.updated_at = time.time()


class Deployer:
    def __init__(self, registry: DeviceRegistry, models: ModelRegistry, token: str = ""):
        self.registry = registry
        self.models = models
        self.token = token
        self._lock = threading.RLock()
        self._deployments: dict[str, Deployment] = {}

    # ---------- reads ----------

    def deployments(self) -> list[dict[str, Any]]:
        with self._lock:
            return [d.to_dict() for d in self._deployments.values()]

    def active_for(self, model: str) -> Optional[Deployment]:
        with self._lock:
            d = self._deployments.get(model)
            return d if d and d.status in ("active", "degraded") else None

    def _busy_ids(self) -> set[str]:
        """Nodes unusable for new plans. Only rpc workers are exclusive in
        v1 (fixed port 50052): a driver node may host more llama-server
        instances on distinct ports, enabling multi-model co-location."""
        with self._lock:
            busy = set()
            for d in self._deployments.values():
                if d.status in ("planning", "provisioning", "active", "degraded"):
                    busy.update(d.worker_device_ids)
            return busy

    def _next_server_port(self) -> int:
        with self._lock:
            return DEFAULT_SERVER_PORT_BASE + len(self._deployments)

    # ---------- writes ----------

    def _pending_reserved_gb(self) -> dict[str, float]:
        """Memory booked by in-flight deployments whose models are not yet
        visible in heartbeats. Prevents over-committing a node while a
        provisioning download/load is underway."""
        with self._lock:
            reserved: dict[str, float] = {}
            for d in self._deployments.values():
                if d.status not in ("provisioning", "planning"):
                    continue
                entry = self.models.get(d.model)
                if entry is None:
                    continue
                for dev_id in [d.driver_device_id, *d.worker_device_ids]:
                    reserved[dev_id] = reserved.get(dev_id, 0.0) + entry.approx_gb
            return reserved

    def deploy(self, model_name: str) -> Deployment:
        entry = self.models.get(model_name)
        if entry is None:
            raise KeyError(model_name)
        if self.active_for(model_name) is not None:
            raise RuntimeError(f"model {model_name} is already deployed")

        devices = [dict(d) for d in self.registry.snapshot() if d["online"]]
        reserved = self._pending_reserved_gb()
        for d in devices:
            hold = reserved.get(d["device_id"], 0.0)
            if hold and "inventory" in d and d["inventory"]:
                d["inventory"] = {
                    **d["inventory"],
                    "mem_available": max(
                        0, int(d["inventory"].get("mem_available", 0) - hold * 1024**3)
                    ),
                }
        plan = plan_placement(
            entry.approx_gb, devices, model_name=model_name, busy_device_ids=self._busy_ids()
        )
        port = self._next_server_port()
        dep = Deployment(
            model=model_name,
            status="provisioning",
            mode=plan.mode,
            driver_device_id=plan.driver_device_id,
            worker_device_ids=plan.worker_device_ids,
            server_port=port,
            reasons=plan.reasons,
        )
        with self._lock:
            self._deployments[model_name] = dep
        threading.Thread(target=self._run, args=(dep, entry, plan), daemon=True).start()
        return dep

    def undeploy(self, model_name: str) -> bool:
        with self._lock:
            dep = self._deployments.get(model_name)
            if dep is None:
                return False
            if dep.status in ("provisioning", "planning"):
                # background thread still owns it; mark and let it observe
                dep.status = "stopping"
                dep.touch()
        threading.Thread(target=self._stop, args=(dep,), daemon=True).start()
        return True

    # ---------- self-healing ----------

    HEAL_GRACE_SECONDS = 20.0   # let flaky Wi-Fi recover before re-planning
    MAX_HEAL_ATTEMPTS = 2
    HEALTH_FAILS_BEFORE_DEGRADED = 3

    def _probe_health(self, base_url: str) -> bool:
        try:
            r = httpx.get(f"{base_url}/health", timeout=3.0)
            return r.status_code == 200
        except httpx.HTTPError:
            return False

    def check_backends(self) -> None:
        """Detect a live node whose llama-server died (or hung): probe
        driver /health each watch tick; N consecutive failures degrade the
        deployment and schedule a heal. Agent supervision restarts crashed
        workers, so this mainly catches 'failed after restart cap' and
        unsupervised leftovers."""
        for snap in self.deployments():
            if snap["status"] != "active" or not snap["driver_base_url"]:
                continue
            model = snap["model"]
            if self._probe_health(snap["driver_base_url"]):
                with self._lock:
                    dep = self._deployments.get(model)
                    if dep is not None:
                        dep.unhealthy_checks = 0
                continue
            with self._lock:
                dep = self._deployments.get(model)
                if dep is None or dep.status != "active":
                    continue
                dep.unhealthy_checks += 1
                if dep.unhealthy_checks < self.HEALTH_FAILS_BEFORE_DEGRADED:
                    continue
                dep.unhealthy_checks = 0
                dep.status = "degraded"
                dep.degraded_cause = "backend_dead"
                dep.error = f"llama-server /health failing at {snap['driver_base_url']}"
                dep.heal_running = True
                dep.touch()
            log.warning("deployment %s: backend health failing; scheduling heal", model)
            threading.Thread(target=self._heal, args=(model,), daemon=True).start()

    def check_and_heal(self) -> None:
        """Called by the degradation watcher. Two transitions:
        active -> degraded when nodes stop heartbeating (schedules a heal),
        degraded -> active when every node's heartbeat returns (the workers
        survived the outage, so the deployment is simply live again)."""
        for snap in self.deployments():
            model = snap["model"]
            nodes = [snap["driver_device_id"], *snap["worker_device_ids"]]
            offline = [n for n in nodes if not self.registry.is_online(n)]

            if snap["status"] == "degraded" and not offline:
                with self._lock:
                    dep = self._deployments.get(model)
                    if (dep is not None and dep.status == "degraded"
                            and dep.degraded_cause == "node_offline"):
                        dep.status = "active"
                        dep.error = ""
                        dep.degraded_cause = ""
                        dep.reasons.append("nodes resumed heartbeating; deployment live again")
                        dep.touch()
                        log.info("deployment %s recovered", model)
                continue

            if snap["status"] != "active" or not offline:
                if snap["status"] == "degraded" and offline:
                    # a previous heal failed and the nodes are still gone:
                    # keep retrying while attempts remain (watch cadence +
                    # grace provide backoff; _heal_running prevents piles)
                    with self._lock:
                        dep = self._deployments.get(model)
                        if (dep is not None and dep.status == "degraded"
                                and not dep.heal_running
                                and dep.heal_attempts < self.MAX_HEAL_ATTEMPTS):
                            dep.heal_running = True
                            threading.Thread(target=self._heal, args=(model,),
                                             daemon=True).start()
                continue
            with self._lock:
                dep = self._deployments.get(model)
                if dep is None or dep.status != "active":
                    continue
                dep.status = "degraded"
                dep.degraded_cause = "node_offline"
                dep.error = f"nodes offline: {', '.join(offline)}"
                dep.heal_running = True
                dep.touch()
            log.warning("deployment %s degraded (%s offline); scheduling heal",
                        model, offline)
            threading.Thread(target=self._heal, args=(model,),
                             daemon=True).start()

    def _heal(self, model: str) -> None:
        try:
            self._heal_inner(model)
        finally:
            with self._lock:
                dep = self._deployments.get(model)
                if dep is not None:
                    dep.heal_running = False

    def _heal_inner(self, model: str) -> None:
        time.sleep(self.HEAL_GRACE_SECONDS)
        with self._lock:
            dep = self._deployments.get(model)
            if dep is None or dep.status != "degraded":
                return  # recovered on its own, or user intervened
            cause = dep.degraded_cause or "node_offline"
            dep.heal_attempts += 1
            attempt = dep.heal_attempts
            # retire the old record so deploy() accepts the model again
            dep.status = "stopped"
            dep.touch()
        log.info("auto-heal %s: attempt %d (cause=%s, re-planning)", model, attempt, cause)
        self._cleanup_partial_local(model)
        try:
            new_dep = self.deploy(model)
        except (KeyError, RuntimeError, PlanError) as exc:
            with self._lock:
                dep = self._deployments.get(model)
                if dep is not None:
                    if dep.heal_attempts >= self.MAX_HEAL_ATTEMPTS:
                        dep.status = "failed"
                    else:
                        dep.status = "degraded"
                    dep.error = f"auto-heal attempt {attempt} failed: {exc}"
                    dep.touch()
            log.warning("auto-heal %s failed: %s", model, exc)
            return
        label = "backend failure" if cause == "backend_dead" else "node loss"
        with self._lock:
            new_dep.reasons.insert(0, f"auto-heal: redeployed after {label} (attempt {attempt})")
            new_dep.heal_attempts = attempt
            new_dep.touch()

    def _cleanup_partial_local(self, model: str) -> None:
        """Best-effort worker cleanup for a deployment record by model name."""
        with self._lock:
            dep = self._deployments.get(model)
        if dep is not None:
            self._cleanup_partial(dep)

    # ---------- background flows ----------

    def _run(self, dep: Deployment, entry, plan) -> None:
        try:
            self._provision(dep, entry, plan)
            dep.status = "active"
            dep.touch()
        except Exception as exc:  # noqa: BLE001 -- deployer must survive agent failures
            dep.status = "failed"
            dep.error = str(exc)
            dep.touch()
            self._cleanup_partial(dep)

    def _provision(self, dep: Deployment, entry, plan) -> None:
        driver = self.registry.get(plan.driver_device_id)
        workers = [self.registry.get(w) for w in plan.worker_device_ids]
        if driver is None or any(w is None for w in workers):
            raise PlanError("a planned node disappeared before provisioning started")

        # 1. engines on every involved node
        for dev in [driver, *workers]:
            self._agent_call(dev.control_addr, "POST", "/engine/ensure", timeout="engine")
            self._set_phase(dep, f"engine ready on {dev.device_id}")

        # 2. rpc workers first so llama-server can connect instantly
        for dev in workers:
            self._agent_call(
                dev.control_addr, "POST", "/workers/rpc",
                json={"port": DEFAULT_RPC_PORT}, timeout="start",
            )
            self._set_phase(dep, f"rpc-server started on {dev.device_id}")

        # 3. model download on the driver node
        hf = os.environ.get("SPARK_HF_ENDPOINT", "")
        r = self._agent_call(
            driver.control_addr, "POST", "/models/download",
            json={"url": entry.url, "filename": entry.filename, "hf_endpoint": hf},
            timeout="download",
        )
        job_id = r.get("job_id")
        if job_id:
            self._poll_download(dep, driver.control_addr, job_id, entry.name)
        self._set_phase(dep, f"model file ready on {driver.device_id}")

        # 4. llama-server on the driver (with rpc chain when splitting)
        rpc_addrs = []
        for dev in workers:
            rpc_addrs.append(f"{dev.host}:{DEFAULT_RPC_PORT}")
        self._agent_call(
            driver.control_addr, "POST", "/workers/server",
            json={
                "model_path": entry.filename,
                "port": dep.server_port,
                "rpc": rpc_addrs,
                "ctx": entry.ctx,
                "threads": max(1, driver.cpu_cores() // 2),
                "extra_args": server_extra_args(driver),
            },
            timeout="start",
        )
        dep.driver_base_url = f"http://{driver.host}:{dep.server_port}"
        self._set_phase(dep, "llama-server started; waiting for /health")

        # 5. health gate
        self._wait_health(dep, dep.driver_base_url)

    def _set_phase(self, dep: Deployment, msg: str) -> None:
        dep.reasons.append(msg)
        dep.touch()

    def _poll_download(self, dep: Deployment, control: str, job_id: str, name: str) -> None:
        deadline = time.time() + 3600
        while time.time() < deadline:
            if dep.status == "stopping":
                raise RuntimeError("deployment cancelled")
            jobs = self._agent_call(control, "GET", "/models/jobs", timeout="job")
            job = next((j for j in jobs if j.get("id") == job_id), None)
            if job is None:
                raise RuntimeError(f"download job {job_id} vanished on agent")
            state = job.get("state")
            if state == "done":
                return
            if state == "error":
                raise RuntimeError(f"model download failed: {job.get('error')}")
            time.sleep(3)
        raise TimeoutError(f"model download for {name} exceeded 1h")

    def _wait_health(self, dep: Deployment, base_url: str) -> None:
        deadline = time.time() + 600
        with httpx.Client(timeout=5.0) as client:
            while time.time() < deadline:
                if dep.status == "stopping":
                    raise RuntimeError("deployment cancelled")
                try:
                    r = client.get(f"{base_url}/health")
                    if r.status_code == 200:
                        return
                except httpx.HTTPError:
                    pass
                time.sleep(2)
        raise TimeoutError(f"llama-server health check failed for {base_url}")

    def _cleanup_partial(self, dep: Deployment) -> None:
        server_ids = [f"server-{dep.server_port}", "server"]
        for dev_id in [dep.driver_device_id, *dep.worker_device_ids]:
            dev = self.registry.get(dev_id)
            if dev is None:
                continue
            for wid in server_ids:
                try:
                    self._agent_call(dev.control_addr, "POST", "/workers/stop",
                                     json={"id": wid}, timeout="start")
                except Exception:  # noqa: BLE001 -- best-effort cleanup
                    pass
            try:
                self._agent_call(dev.control_addr, "POST", "/workers/stop",
                                 json={"id": "rpc"}, timeout="start")
            except Exception:  # noqa: BLE001
                pass

    def _stop(self, dep: Deployment) -> None:
        self._cleanup_partial(dep)
        dep.status = "stopped"
        dep.touch()

    # ---------- agent transport ----------

    def _agent_call(self, control_addr: str, method: str, path: str, *, json=None,
                    timeout: str = "status") -> Any:
        headers = {}
        if self.token:
            headers["X-SPARK-Token"] = self.token
        url = f"http://{control_addr}{path}"
        with httpx.Client(timeout=AGENT_TIMEOUTS[timeout], headers=headers) as client:
            r = client.request(method, url, json=json)
        if r.status_code >= 400:
            raise RuntimeError(f"agent {control_addr} {path} -> HTTP {r.status_code}: {r.text[:300]}")
        if r.status_code == 204 or not r.content:
            return {}
        return r.json()
