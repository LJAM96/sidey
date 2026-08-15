#!/usr/bin/env python3
"""Open an RSD tunnel to a paired Apple TV over the network (no USB).

Uses the same RemotePairing mechanism as scripts/wireless-tunnel.py but for
the Apple TV, using the proven Phase G flow:

  * `RemotePairingTunnelService.connect(autopair=False)` - uses the existing
    pair record; `autopair=True` (create_core_device_tunnel_service_using_
    remotepairing) hung during Phase G, so the explicit autopair=False
    path is required.
  * `start_tunnel_over_remotepairing(..., protocol=TCP)` - the TCP protocol,
    not the default QUIC.

The pair record is network-scoped: it must be created (manual RemotePairing
PIN pairing) on the host that runs this tunnel. A record created over the LAN
is rejected by pair verify over the tailnet (`ERROR 0x02`).

The tunnel needs root for the TUN device; pair-verify/install run as an
unprivileged user.

Usage:
  tvos-tunnel.py --udid <UDID> --address <TAILNET_IP> [--port 49152]

Prints "HOST PORT" once the tunnel is up (and writes it to --endpoint-file),
then keeps the tunnel alive.
"""

import argparse
import asyncio

from pymobiledevice3.remote.common import TunnelProtocol
from pymobiledevice3.remote.tunnel_service import (
    RemotePairingTunnelService,
    start_tunnel_over_remotepairing,
)

DEFAULT_PORT = 49152  # RemotePairing daemon port (advertised via Bonjour)


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", required=True, help="RemotePairing identifier from the pair record")
    parser.add_argument("--address", required=True, help="device address reachable from this host (tailnet IP)")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help="RemotePairing daemon port")
    parser.add_argument("--endpoint-file", default="/run/sidey/tvs-endpoint", help="file to write 'HOST PORT' into once the tunnel is up")
    args = parser.parse_args()

    service = RemotePairingTunnelService(args.udid, args.address, args.port)
    await service.connect(autopair=False)
    print("VERIFY OK", flush=True)
    async with start_tunnel_over_remotepairing(service, protocol=TunnelProtocol.TCP) as result:
        endpoint = f"{result.address} {result.port}"
        print(f"TUNNEL {endpoint}", flush=True)
        try:
            with open(args.endpoint_file, "w") as f:
                f.write(endpoint)
        except OSError as e:
            print(f"warning: could not write {args.endpoint_file}: {e}", flush=True)
        while True:
            await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())