#!/usr/bin/env python3
"""Open an RSD tunnel to a paired iPhone over the network (no USB).

Uses the RemotePairing channel with the RPPairing record created once over
USB by:
  pymobiledevice3 lockdown wifi-connections --state on
  pymobiledevice3 lockdown remotepairing --pair

Discovery in pymobiledevice3 is Bonjour-based (does not cross the tailnet),
so the device address is passed explicitly.

Usage:
  wireless-tunnel.py --udid <UDID> --address <TAILNET_IP> [--port 49152]

Prints "HOST PORT" once the tunnel is up (and writes it to --endpoint-file),
then keeps the tunnel alive.
"""

import argparse
import asyncio

from pymobiledevice3.remote.common import TunnelProtocol
from pymobiledevice3.remote.tunnel_service import RemotePairingTunnelService, start_tunnel


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", required=True)
    parser.add_argument("--address", required=True, help="device address reachable from this host (tailnet IP)")
    parser.add_argument("--port", type=int, default=49152, help="RemotePairing daemon port (advertised via Bonjour; 62078 is the legacy lockdown wifi listener)")
    parser.add_argument("--endpoint-file", default="/run/sidey/rsd-endpoint", help="file to write 'HOST PORT' into once the tunnel is up")
    args = parser.parse_args()

    service = RemotePairingTunnelService(args.udid, args.address, args.port)
    await service.connect()
    async with start_tunnel(service, protocol=TunnelProtocol.TCP) as tunnel_result:
        endpoint = f"{tunnel_result.address} {tunnel_result.port}"
        print(endpoint, flush=True)
        try:
            with open(args.endpoint_file, "w") as f:
                f.write(endpoint)
        except OSError as e:
            print(f"warning: could not write {args.endpoint_file}: {e}", flush=True)
        while True:
            await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())
