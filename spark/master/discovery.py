"""Master-side UDP discovery: listens for agent announces, broadcasts beacons.

Runs on a daemon thread with one UDP socket bound to UDP_MASTER_PORT:
  - inbound:  SPARK/1 ANNOUNCE <json>  -> registry.upsert
  - outbound: SPARK/1 MASTER <json>    -> subnet broadcasts to agent port
"""

from __future__ import annotations

import json
import logging
import socket
import struct
import threading
import time

from spark.master.registry import DeviceRegistry
from spark.protocol import PACKET_ANNOUNCE, PACKET_MASTER, UDP_AGENT_PORT, UDP_MASTER_PORT

log = logging.getLogger(__name__)


def _subnet_broadcasts() -> list[str]:
    """Candidate broadcast addresses for the master beacon.

    The stdlib cannot read interface netmasks, so we send both the global
    broadcast and /24-derived subnet broadcasts (covers typical home LANs;
    agents can always fall back to --master / SPARK_MASTER if an AP drops
    broadcasts).
    """
    out = {"255.255.255.255"}
    try:
        for info in socket.getaddrinfo(socket.gethostname(), None, socket.AF_INET):
            ip = info[4][0]
            if ip.startswith("127.") or ip in out:
                continue
            packed_ip = struct.unpack("!I", socket.inet_aton(ip))[0]
            packed_mask = struct.unpack("!I", socket.inet_aton("255.255.255.0"))[0]
            bc = socket.inet_ntoa(struct.pack("!I", packed_ip | (~packed_mask & 0xFFFFFFFF)))
            out.add(bc)
    except OSError:
        pass
    return sorted(out)


class DiscoveryListener:
    def __init__(self, registry: DeviceRegistry, master_port_http: int, master_host: str = ""):
        self.registry = registry
        self.master_http = f"{master_host or _local_ip()}:{master_port_http}"
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        if self._thread is not None:
            return
        self._thread = threading.Thread(target=self._loop, name="spark-discovery", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()

    def _loop(self) -> None:
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
        try:
            sock.bind(("0.0.0.0", UDP_MASTER_PORT))
        except OSError as exc:
            log.error("cannot bind UDP %d (%s); agents must use --master", UDP_MASTER_PORT, exc)
            return
        log.info("discovery listening on UDP %d, master beacon %s", UDP_MASTER_PORT, self.master_http)

        sock.settimeout(1.0)
        last_beacon = 0.0
        while not self._stop.is_set():
            try:
                data, _addr = sock.recvfrom(65536)
            except socket.timeout:
                pass
            except OSError:
                break
            else:
                self._handle(data)
            now = time.time()
            if now - last_beacon >= 5.0:
                self._beacon(sock)
                last_beacon = now
        sock.close()

    def _handle(self, data: bytes) -> None:
        try:
            text = data.decode("utf-8", errors="replace")
            if not text.startswith(PACKET_ANNOUNCE):
                return
            announce = json.loads(text[len(PACKET_ANNOUNCE):])
            self.registry.upsert(announce)
        except (ValueError, KeyError) as exc:
            log.debug("bad announce packet: %s", exc)

    def _beacon(self, sock: socket.socket) -> None:
        payload = PACKET_MASTER + json.dumps(
            {"protocol": 1, "master_addr": self.master_http, "ts": int(time.time())}
        )
        for bc in _subnet_broadcasts():
            try:
                sock.sendto(payload.encode("utf-8"), (bc, UDP_AGENT_PORT))
            except OSError:
                continue


def _local_ip() -> str:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("223.5.5.5", 53))  # no packets sent; just picks a route
        return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        s.close()
