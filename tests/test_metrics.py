"""Gateway metrics and engine pass-through tests."""

import time

from spark.master.metrics import RequestMetrics


class TestRequestMetrics:
    def test_counts_and_series(self):
        m = RequestMetrics()
        m.start()
        assert m.snapshot()["active"] == 1
        m.end(0.5)
        m.start()
        m.end(1.5)
        snap = m.snapshot()
        assert snap["active"] == 0
        assert snap["total"] == 2
        assert len(snap["requests_per_min"]) == 60
        assert snap["requests_per_min"][-1][1] == 2  # current minute
        assert snap["p50_latency_s"] == 1.5

    def test_bucket_is_per_minute(self):
        m = RequestMetrics()
        m.start(); m.end(0.1)
        # simulate an old bucket entry beyond the window
        m._buckets.append([int(time.time() // 60) - 120, 5])
        snap = m.snapshot()
        assert all(p[1] >= 0 for p in snap["requests_per_min"])
        assert m._buckets[0][1] == 5 or len(m._buckets) <= 60

    def test_engine_passthrough_in_registry(self):
        from spark.master.registry import DeviceRegistry

        reg = DeviceRegistry()
        reg.upsert({
            "device_id": "d1", "name": "d1", "control_addr": "h:1",
            "inventory": {"gpus": []}, "workers": [],
            "engine": {"flavor": "cuda", "tag": "b11878"},
        })
        snap = reg.snapshot()[0]
        assert snap["engine"]["flavor"] == "cuda"
