"""API smoke tests with a mocked deployer transport (no real agents)."""

import time

from fastapi.testclient import TestClient

from spark.master.app import create_app
from spark.master.deployer import Deployer
from spark.master.registry import DeviceRegistry
from spark.modelreg import ModelRegistry


def make_client():
    registry = DeviceRegistry()
    models = ModelRegistry.load()
    deployer = Deployer(registry, models)
    app = create_app(registry=registry, models=models, deployer=deployer,
                     start_discovery=False)
    return TestClient(app), registry


def heartbeat(client, device_id, free_gb=4):
    return client.post("/api/heartbeat", json={
        "device_id": device_id,
        "name": device_id,
        "control_addr": f"{device_id}-host:3971",
        "inventory": {"mem_available": int(free_gb * 1024**3),
                       "mem_total": int(free_gb * 1024**3),
                       "cpu_cores": 8, "gpus": []},
        "workers": [],
        "ts": int(time.time()),
    })


class TestClusterAPI:
    def test_heartbeat_registers_device(self):
        client, _ = make_client()
        assert heartbeat(client, "d1").status_code == 200
        devs = client.get("/api/devices").json()
        assert [d["device_id"] for d in devs] == ["d1"]
        assert devs[0]["online"] is True

    def test_heartbeat_rejects_missing_id(self):
        client, _ = make_client()
        assert client.post("/api/heartbeat", json={"name": "x"}).status_code == 400

    def test_models_endpoint_lists_registry(self):
        client, _ = make_client()
        names = [m["name"] for m in client.get("/api/models").json()]
        assert "qwen2.5-3b-instruct" in names

    def test_deploy_unknown_model_404(self):
        client, _ = make_client()
        r = client.post("/api/deploy", json={"model": "nope"})
        assert r.status_code == 404

    def test_deploy_without_devices_409(self):
        client, _ = make_client()
        r = client.post("/api/deploy", json={"model": "qwen2.5-3b-instruct"})
        assert r.status_code == 409
        assert "no available devices" in r.json()["detail"]

    def test_undeploy_missing_404(self):
        client, _ = make_client()
        assert client.post("/api/undeploy", json={"model": "x"}).status_code == 404


class TestOpenAIGateway:
    def test_v1_models_empty_then_listed(self, monkeypatch):
        client, registry = make_client()
        heartbeat(client, "d1")
        assert client.get("/v1/models").json()["data"] == []

        from spark.master.deployer import Deployment

        dep = Deployment(model="qwen2.5-3b-instruct", status="active",
                         driver_device_id="d1", driver_base_url="http://x:8300")
        monkeypatch.setattr(
            client.app.state.deployer, "deployments",
            lambda: [dep.to_dict()])
        monkeypatch.setattr(
            client.app.state.deployer, "active_for",
            lambda model: dep if model == dep.model else None)
        data = client.get("/v1/models").json()["data"]
        assert data == [{"id": "qwen2.5-3b-instruct", "object": "model", "owned_by": "spark"}]

    def test_chat_completion_model_not_deployed_404(self):
        client, _ = make_client()
        r = client.post("/v1/chat/completions",
                        json={"model": "qwen2.5-3b-instruct", "messages": []})
        assert r.status_code == 404
        assert "not deployed" in r.json()["detail"]

    def test_dashboard_served(self):
        client, _ = make_client()
        r = client.get("/")
        assert r.status_code == 200
        assert "SPARK" in r.text
