"""Placement planner unit tests: the core tiered-placement rules."""

import pytest

from spark.master.planner import PlanError, plan_placement


def dev(device_id, free_gb, total_gb=None, gpu=False, cores=8, host=None):
    return {
        "device_id": device_id,
        "control_addr": f"{host or device_id}:3971",
        "inventory": {
            "mem_available": int(free_gb * 1024**3),
            "mem_total": int((total_gb or free_gb) * 1024**3),
            "cpu_cores": cores,
            "gpus": [{"name": "x", "vram_bytes": 1}] if gpu else [],
        },
    }


class TestSingleMode:
    def test_small_model_goes_to_best_single_node(self):
        devices = [dev("a", 2), dev("b", 8), dev("c", 4)]
        plan = plan_placement(1.1, devices, "m")
        assert plan.mode == "single"
        assert plan.driver_device_id == "b"
        assert plan.worker_device_ids == []

    def test_gpu_node_wins_when_memory_comparable(self):
        devices = [dev("cpu-node", 8.0), dev("gpu-node", 7.9, gpu=True)]
        plan = plan_placement(1.0, devices, "m")
        assert plan.driver_device_id == "gpu-node"

    def test_busy_nodes_are_excluded(self):
        devices = [dev("a", 8), dev("b", 4)]
        plan = plan_placement(1.0, devices, "m", busy_device_ids=["a"])
        assert plan.driver_device_id == "b"

    def test_just_above_threshold_fits_single(self):
        # need = 1.15*approx + 0.5 overhead; a hair above must fit
        devices = [dev("a", 1.15 * 4 + 0.5 + 0.01)]
        plan = plan_placement(4.0, devices, "m")
        assert plan.mode == "single"

    def test_just_below_threshold_goes_split(self):
        devices = [dev("a", 1.15 * 4 + 0.5 - 0.2), dev("b", 2)]
        plan = plan_placement(4.0, devices, "m")
        assert plan.mode == "split"
        assert plan.driver_device_id == "a"
        assert plan.worker_device_ids == ["b"]


class TestSplitMode:
    def test_big_model_splits_across_nodes(self):
        devices = [dev("a", 3), dev("b", 3), dev("c", 2)]
        plan = plan_placement(5.0, devices, "m")
        assert plan.mode == "split"
        # need 6.25 cumulative: a(3)+b(3)=6 < 6.25, plus c -> 8
        assert plan.driver_device_id == "a"
        assert set(plan.worker_device_ids) == {"b", "c"}
        assert set(plan.node_ids) == {"a", "b", "c"}

    def test_split_picks_fewest_biggest_nodes(self):
        devices = [dev("a", 4), dev("b", 3), dev("c", 3), dev("d", 3)]
        plan = plan_placement(5.0, devices, "m")
        # need 6.25 cumulative: a(4)+b(3)=7 already covers it
        assert plan.driver_device_id == "a"
        assert plan.worker_device_ids == ["b"]
        assert "c" not in plan.node_ids and "d" not in plan.node_ids

    def test_insufficient_memory_raises(self):
        devices = [dev("a", 1), dev("b", 1)]
        with pytest.raises(PlanError, match="cannot host"):
            plan_placement(5.0, devices, "m")

    def test_no_devices_raises(self):
        with pytest.raises(PlanError, match="no available devices"):
            plan_placement(1.0, [], "m")
