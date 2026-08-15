#!/usr/bin/env python3
"""Install a (signed) IPA on an Apple TV over an existing RSD tunnel.

Companion to scripts/tvos-tunnel.py. The tunnel daemon (run as root for the
TUN device) writes the RSD endpoint ("HOST PORT") into --endpoint-file; this
script connects to that endpoint as an unprivileged user and drives
InstallationProxy.

Follows the proven Phase G flow:
  RemoteServiceDiscoveryService((addr, port)).connect() then
  InstallationProxyService(lockdown=rsd).install_from_local(...)

After the install completes the bundle is looked up via `get_apps` and the
result is reported so installers can verify centrally.

Usage:
  tvos-install.py --ipa PATH --bundle-identifier BUNDLE_ID
                  [--endpoint-file /run/sidey/tvs-endpoint]
"""

import argparse
import asyncio
import sys
from pathlib import Path

from pymobiledevice3.remote.remote_service_discovery import RemoteServiceDiscoveryService
from pymobiledevice3.services.installation_proxy import InstallationProxyService


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ipa", required=True, help="signed IPA to install")
    parser.add_argument("--bundle-identifier", required=True, help="bundle id of the IPA (for post-install verify)")
    parser.add_argument("--endpoint-file", default="/run/sidey/tvs-endpoint", help="RSD endpoint file written by tvos-tunnel.py")
    args = parser.parse_args()

    with open(args.endpoint_file) as f:
        host, port = f.read().split()
    port = int(port)

    rsd = RemoteServiceDiscoveryService((host, port))
    await rsd.connect()
    os_version = rsd.peer_info.get("Properties", {}).get("OSVersion", "?")
    print(f"RSD CONNECTED os={os_version}", flush=True)

    proxy = InstallationProxyService(lockdown=rsd)

    def prog(p, s, **kw):
        print(f"{p}% {s}", flush=True)

    await proxy.install_from_local(Path(args.ipa), handler=prog)
    print("INSTALL COMPLETE", flush=True)

    # Verify the bundle is present on the device.
    apps = await proxy.get_apps(bundle_identifiers=[args.bundle_identifier])
    if args.bundle_identifier in apps:
        print(f"FOUND {args.bundle_identifier}", flush=True)
    else:
        print(f"MISSING {args.bundle_identifier}", flush=True)


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except Exception as e:  # noqa: BLE001
        import traceback

        traceback.print_exc()
        sys.exit(1)