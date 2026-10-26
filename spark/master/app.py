"""FastAPI application: cluster API + OpenAI-compatible gateway + dashboard."""

from __future__ import annotations

import asyncio
import logging
import os
import time
from pathlib import Path
from contextlib import asynccontextmanager
from typing import Any, Optional

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse

from spark.master.deployer import Deployer
from spark.master.discovery import DiscoveryListener
from spark.master.metrics import RequestMetrics
from spark.master.planner import PlanError
from spark.master.registry import DeviceRegistry
from spark.master import wol
from spark.modelreg import ModelRegistry
from spark.protocol import DEFAULT_MASTER_PORT

log = logging.getLogger(__name__)

DASHBOARD = Path(__file__).parent / "dashboard" / "index.html"


def create_app(
    *,
    registry: Optional[DeviceRegistry] = None,
    models: Optional[ModelRegistry] = None,
    deployer: Optional[Deployer] = None,
    start_discovery: bool = True,
    http_port: int = DEFAULT_MASTER_PORT,
    token: str = "",
) -> FastAPI:
    token = token or os.environ.get("SPARK_TOKEN", "")
    registry = registry or DeviceRegistry()
    models = models or ModelRegistry.load()
    if deployer is None:
        default_state = os.environ.get(
            "SPARK_STATE_FILE",
            str(Path.home() / ".spark-master" / "deployments.json"),
        )
        deployer = Deployer(registry, models, token=token, state_path=default_state)
    else:
        deployer.token = deployer.token or token

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.client = httpx.AsyncClient(timeout=httpx.Timeout(600.0, connect=5.0))
        try:
            deployer.reconcile()
        except Exception:  # noqa: BLE001 -- adoption must not block startup
            log.exception("deployment reconcile failed")
        listener = DiscoveryListener(registry, http_port, token_required=bool(token)) if start_discovery else None
        if listener:
            listener.start()
        watcher = asyncio.create_task(_degradation_watch(deployer, registry))
        idle_min = float(os.environ.get("SPARK_IDLE_SLEEP_MIN", "0") or 0)
        policy = None
        if idle_min > 0:
            from spark.master.power import IdlePolicy

            policy = IdlePolicy(deployer, registry, idle_min)
            log.info("idle auto-sleep enabled: %.0f min", idle_min)
        idle_watcher = asyncio.create_task(_idle_watch(policy)) if policy else None
        yield
        watcher.cancel()
        if idle_watcher:
            idle_watcher.cancel()
        if listener:
            listener.stop()
        await app.state.client.aclose()

    app = FastAPI(title="SPARK On-premises master", lifespan=lifespan)
    app.state.registry = registry
    app.state.models = models
    app.state.deployer = deployer
    app.state.metrics = RequestMetrics()

    # ---------------- cluster management ----------------

    @app.post("/api/heartbeat")
    async def heartbeat(req: Request) -> dict:
        if token and req.headers.get("X-SPARK-Token") != token:
            raise HTTPException(401, "bad or missing X-SPARK-Token")
        try:
            registry.upsert(await req.json(), observed_ip=req.client.host if req.client else "")
        except (ValueError, KeyError) as exc:
            raise HTTPException(400, str(exc))
        return {"ok": True}

    @app.get("/api/devices")
    async def devices() -> list[dict]:
        return registry.snapshot()

    @app.get("/api/models")
    async def list_models() -> list[dict]:
        return [
            {
                "name": m.name,
                "approx_gb": m.approx_gb,
                "ctx": m.ctx,
                "desc": m.desc,
                "url": m.url,
            }
            for m in models.as_list()
        ]

    @app.get("/api/deployments")
    async def deployments() -> list[dict]:
        return deployer.deployments()

    @app.post("/api/deploy")
    async def deploy(req: Request) -> dict:
        body = await req.json()
        name = body.get("model", "")
        if models.get(name) is None:
            raise HTTPException(404, f"unknown model {name!r}; see /api/models")
        try:
            dep = deployer.deploy(name)
        except KeyError:
            raise HTTPException(404, f"unknown model {name!r}")
        except (RuntimeError, PlanError) as exc:
            raise HTTPException(409, str(exc))
        return dep.to_dict()

    @app.post("/api/undeploy")
    async def undeploy(req: Request) -> dict:
        body = await req.json()
        name = body.get("model", "")
        if not deployer.undeploy(name):
            raise HTTPException(404, f"no deployment for {name!r}")
        return {"model": name, "status": "stopping"}

    @app.get("/api/summary")
    async def summary() -> dict:
        devs = registry.snapshot()
        deps = deployer.deployments()
        return {
            "ts": time.time(),
            "devices_online": sum(1 for d in devs if d["online"]),
            "devices_total": len(devs),
            "deployments_active": sum(1 for d in deps if d["status"] == "active"),
            "devices": devs,
            "deployments": deps,
            "models": [m["name"] for m in await list_models()],
            "metrics": app.state.metrics.snapshot(),
        }

    @app.post("/api/wake")
    async def wake(req: Request) -> dict:
        body = await req.json()
        dev = registry.get(body.get("device_id", ""))
        if dev is None:
            raise HTTPException(404, "unknown device")
        macs = (dev.inventory or {}).get("macs") or []
        if not macs:
            raise HTTPException(409, f"device {dev.device_id} reported no MAC addresses")
        sent = wol.send_wol(macs[0], broadcast=wol.subnet_broadcast(dev.host))
        if sent == 0:
            raise HTTPException(502, "magic packet could not be sent (check LAN/broadcast)")
        return {"device_id": dev.device_id, "mac": macs[0], "bytes_sent": sent,
                "note": "唤醒需设备 BIOS/网卡开启 Wake-on-LAN；唤醒后 agent 会自动重新心跳"}

    @app.post("/api/sleep")
    async def sleep(req: Request) -> dict:
        body = await req.json()
        dev = registry.get(body.get("device_id", ""))
        if dev is None:
            raise HTTPException(404, "unknown device")
        try:
            deployer._agent_call(dev.control_addr, "POST", "/power/sleep", timeout="status")
        except RuntimeError as exc:
            raise HTTPException(409, str(exc))
        return {"device_id": dev.device_id, "status": "sleep command sent"}

    # ---------------- OpenAI-compatible gateway ----------------

    @app.get("/v1/models")
    async def v1_models() -> dict:
        deps = [d for d in deployer.deployments() if d["status"] == "active"]
        return {
            "object": "list",
            "data": [
                {"id": d["model"], "object": "model", "owned_by": "spark"} for d in deps
            ],
        }

    async def _route_to_driver(model: str) -> str:
        dep = deployer.active_for(model)
        if dep is None:
            available = [d["model"] for d in deployer.deployments() if d["status"] == "active"]
            raise HTTPException(
                404,
                f"model {model!r} is not deployed; active models: {available or 'none'}; "
                f"POST /api/deploy to start one",
            )
        if dep.status == "degraded":
            raise HTTPException(503, f"model {model!r} is degraded (node offline); retry later")
        return dep.driver_base_url

    async def _proxy(request: Request, model: str, path: str) -> StreamingResponse:
        base = await _route_to_driver(model)
        url = f"{base}{path}"
        body = await request.body()
        headers = {k: v for k, v in request.headers.items()
                   if k.lower() in ("content-type", "accept", "authorization")}
        client: httpx.AsyncClient = request.app.state.client
        metrics: RequestMetrics = request.app.state.metrics

        t0 = time.time()
        metrics.start()
        try:
            upstream = await client.send(
                client.build_request(request.method, url, content=body, headers=headers),
                stream=True,
            )
        except httpx.HTTPError:
            metrics.end(time.time() - t0)
            raise

        async def relay():
            try:
                async for chunk in upstream.aiter_raw():
                    yield chunk
            finally:
                await upstream.aclose()
                metrics.end(time.time() - t0)

        resp_headers = {
            k: v for k, v in upstream.headers.items()
            if k.lower() in ("content-type", "cache-control")
        }
        return StreamingResponse(relay(), status_code=upstream.status_code,
                                 headers=resp_headers, background=None)

    @app.post("/v1/chat/completions")
    async def chat_completions(request: Request):
        body = await request.json()
        model = body.get("model", "")
        return await _proxy(request, model, "/v1/chat/completions")

    @app.post("/v1/completions")
    async def completions(request: Request):
        body = await request.json()
        model = body.get("model", "")
        return await _proxy(request, model, "/v1/completions")

    # ---------------- dashboard ----------------

    @app.get("/")
    async def index() -> FileResponse:
        return FileResponse(DASHBOARD)

    @app.exception_handler(httpx.HTTPError)
    async def upstream_error(request: Request, exc: httpx.HTTPError):
        return JSONResponse(status_code=502, content={"error": f"upstream unreachable: {exc}"})

    return app


async def _degradation_watch(deployer: Deployer, registry: DeviceRegistry) -> None:
    """Two-level health watch: probe live backends, then let the deployer
    handle node loss / recovery and schedule auto-heals."""
    while True:
        await asyncio.sleep(10)
        try:
            deployer.check_backends()
        except Exception:  # noqa: BLE001 -- watcher must survive odd states
            log.exception("backend health check failed")
        deployer.check_and_heal()
        deployer.persist()


async def _idle_watch(policy) -> None:
    while True:
        await asyncio.sleep(10)
        try:
            policy.tick()
        except Exception:  # noqa: BLE001
            log.exception("idle policy tick failed")
