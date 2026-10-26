"""Bandwidth-aware split gating tests (no real network)."""

import pytest

from spark.master.netprobe import NetStat
from spark.master.planner import PlanError, PlacementPlan, validate_split_network


def stat(device_id, mbps, rtt, ok=True):
    return NetStat(device_id=device_id, mbps=mbps, rtt_ms=rtt, ok=ok)


def split_plan(workers):
    return PlacementPlan(mode="split", driver_device_id="driver", worker_device_ids=workers)


class TestLinkClass:
    def test_buckets(self):
        assert stat("a", 940, 0.4).link_class == "wired"
        assert stat("a", 200, 3).link_class == "wifi"
        assert stat("a", 40, 8).link_class == "slow"
        assert stat("a", 5, 40).link_class == "poor"
        assert stat("a", 0, 0, ok=False).link_class == "unknown"

    def test_wired_requires_low_rtt(self):
        assert stat("a", 940, 30).link_class == "wifi"  # fast but high latency


class TestValidateSplitNetwork:
    def test_poor_link_rejects_split(self):
        with pytest.raises(PlanError, match="network too slow"):
            validate_split_network(split_plan(["w1"]), {"w1": stat("w1", 8, 35)})

    def test_wifi_allows_with_warning(self):
        notes = validate_split_network(split_plan(["w1"]), {"w1": stat("w1", 120, 4)})
        assert any("may be limited" in n for n in notes)

    def test_wired_is_clean_note(self):
        notes = validate_split_network(split_plan(["w1"]), {"w1": stat("w1", 900, 0.5)})
        assert len(notes) == 1 and "wired" in notes[0]

    def test_unknown_probe_passes_with_note(self):
        notes = validate_split_network(split_plan(["w1"]), {})
        assert any("unverified" in n for n in notes)

    def test_single_mode_untouched(self):
        plan = PlacementPlan(mode="single", driver_device_id="d")
        assert validate_split_network(plan, {}) == []

    def test_one_poor_worker_among_good_rejects(self):
        with pytest.raises(PlanError):
            validate_split_network(
                split_plan(["w1", "w2"]),
                {"w1": stat("w1", 900, 0.5), "w2": stat("w2", 3, 50)},
            )
