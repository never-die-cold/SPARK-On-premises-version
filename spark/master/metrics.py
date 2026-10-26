"""In-memory gateway metrics (requests/min, active streams, latency).

Deliberately tiny: counts proxied requests and their wall-clock duration.
Token-level throughput stays in benchmarks/ (spark bench) where it is
measured precisely; this feeds the live dashboard only.
"""

from __future__ import annotations

import collections
import threading
import time


class RequestMetrics:
    WINDOW_MINUTES = 60

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._buckets: collections.deque = collections.deque()  # [minute, count]
        self._latencies: collections.deque = collections.deque(maxlen=200)
        self.active = 0
        self.total = 0

    def start(self) -> None:
        with self._lock:
            self.active += 1

    def end(self, seconds: float) -> None:
        with self._lock:
            self.active = max(0, self.active - 1)
            self.total += 1
            minute = int(time.time() // 60)
            if self._buckets and self._buckets[-1][0] == minute:
                self._buckets[-1][1] += 1
            else:
                self._buckets.append([minute, 1])
            while len(self._buckets) > self.WINDOW_MINUTES:
                self._buckets.popleft()
            self._latencies.append(seconds)

    def snapshot(self) -> dict:
        with self._lock:
            now_min = int(time.time() // 60)
            counts = {m: c for m, c in self._buckets}
            series = [
                [now_min - (self.WINDOW_MINUTES - 1) + i,
                 counts.get(now_min - (self.WINDOW_MINUTES - 1) + i, 0)]
                for i in range(self.WINDOW_MINUTES)
            ]
            lat = sorted(self._latencies)
            p50 = lat[len(lat) // 2] if lat else 0.0
            return {
                "active": self.active,
                "total": self.total,
                "p50_latency_s": round(p50, 3),
                "requests_per_min": series,
            }
