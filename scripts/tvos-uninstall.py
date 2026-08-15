#!/usr/bin/env python3
"""Uninstall an app from an Apple TV over an existing RSD tunnel.

Companion to scripts/tvos-tunnel.py; pairs with the tunnel endpoint file in
the same way tvos-install.py does. Drives
InstallationProxyService.uninstall for the given bundle id.

Usage:
  tvos-uninstall.py --bundle-identifier BUNDLE_ID
                    [--endpoint-file /run/sidey/tvs-endpoint]
"""

import argparse
import asyncio
import sys

from pymobiledevice3.remote.remote_service_discovery import RemoteServiceDiscoveryService
from pymobiledevice3.services.installation_proxy import InstallationProxyService


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle-identifier", required=True, help="bundle id to remove")
    parser.add_argument("--endpoint-file", default="/run/sidey/tvs-endpoint", help="RSD endpoint file written by tvos-tunnel.py")
    args = parser.parse_args()

    with open(args.endpoint_file) as f:
        host, port = f.read().split()
    port = int(port)

    rsd = RemoteServiceDiscoveryService((host, port))
    await rsd.connect()

    proxy = InstallationProxyService(lockdown=rsd)

    def prog(p, s, **kw):
        print(f"{p}% {s}", flush=True)

    await proxy.uninstall(args.bundle_identifier, handler=prog)
    print("UNINSTALL COMPLETE", flush=True)

    # Verify the bundle is gone from the device.
    apps = await proxy.get_apps(bundle_identifiers=[args.bundle_identifier])
    if args.bundle_identifier in apps:
        print(f"STILL PRESENT {args.bundle_identifier}", flush=True)
    else:
        print(f"GONE {args.bundle_identifier}", flush=True)


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except Exception as e:  # noqa: BLE001
        import traceback

        traceback.print_exc()
        sys.exit(1)