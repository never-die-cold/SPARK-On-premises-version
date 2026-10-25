"""Wake-on-LAN: magic packet construction and LAN delivery."""

from __future__ import annotations

import logging
import socket
import struct

log = logging.getLogger(__name__)


def magic_packet(mac: str) -> bytes:
    """6x 0xFF followed by the target MAC repeated 16 times."""
    clean = "".join(c for c in mac if c in "0123456789abcdefABCDEF")
    if len(clean) != 12:
        raise ValueError(f"bad MAC address: {mac!r}")
    addr = bytes.fromhex(clean)
    return b"\xff" * 6 + addr * 16


def send_wol(mac: str, broadcast: str = "255.255.255.255", port: int = 9) -> int:
    """Send a WoL magic packet; returns bytes sent (0 on failure)."""
    packet = magic_packet(mac)
    # some NICs only wake on the subnet-directed broadcast; send both
    targets = {broadcast, "255.255.255.255"}
    sent = 0
    for target in targets:
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
                s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
                s.settimeout(2)
                sent += s.sendto(packet, (target, port))
        except OSError as exc:
            log.debug("wol send to %s failed: %s", target, exc)
    return sent


def subnet_broadcast(ip: str) -> str:
    """/24 subnet broadcast for a LAN ip (WoL fallback target)."""
    try:
        packed = struct.unpack("!I", socket.inet_aton(ip))[0]
        mask = struct.unpack("!I", socket.inet_aton("255.255.255.0"))[0]
        return socket.inet_ntoa(struct.pack("!I", packed | (~mask & 0xFFFFFFFF)))
    except OSError:
        return "255.255.255.255"
