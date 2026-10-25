"""Shared protocol constants for SPARK On-premises."""

PROTO_VERSION = 1
UDP_AGENT_PORT = 3970   # agents listen here for master beacons
UDP_MASTER_PORT = 3969  # master listens here for agent announces
PACKET_ANNOUNCE = "SPARK/1 ANNOUNCE "
PACKET_MASTER = "SPARK/1 MASTER "

DEFAULT_MASTER_PORT = 8000
DEFAULT_RPC_PORT = 50052
DEFAULT_SERVER_PORT_BASE = 8300

# Placement planning multipliers (engineering defaults, see
# docs/architecture.md -- not yet calibrated by benchmarks).
SINGLE_FIT_FACTOR = 1.15
SPLIT_FIT_FACTOR = 1.25
CTX_OVERHEAD_GB = 0.5
