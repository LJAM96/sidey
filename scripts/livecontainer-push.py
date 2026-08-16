#!/usr/bin/env python3
"""
livecontainer-push.py: Push a raw guest IPA (or certificate p12) directly into
LiveContainer's container on an iOS device using pymobiledevice3
HouseArrestService over USB or the wireless RSD tunnel.

Usage:
  python3 livecontainer-push.py --udid <UDID> --file /path/to/app.ipa [--bundle com.kdt.livecontainer]
  python3 livecontainer-push.py --endpoint-file /run/sidey/rsd-endpoint --file /path/to/cert.p12

Requires pymobiledevice3 >= 10.3 (RSD HouseArrest shim support).
"""

import argparse
import asyncio
import os
import sys

BUNDLE_DEFAULT = "com.kdt.livecontainer"


async def _rsd_provider(endpoint_file: str):
    from pymobiledevice3.remote.remote_service_discovery import RemoteServiceDiscoveryService

    with open(endpoint_file) as f:
        host, port = f.read().strip().split()[:2]
    rsd = RemoteServiceDiscoveryService((host, int(port)))
    await rsd.connect()
    return rsd


async def _usbmux_provider(udid: str):
    from pymobiledevice3.lockdown import create_using_usbmux

    return await create_using_usbmux(udid)


async def _installed_livecontainer_bundle(provider) -> str | None:
    """Resolve the real installed bundle id for a sideloaded LiveContainer.

    isideload shadows the host bundle id by appending the team id, and
    historically appended it twice (LiveContainer shows as
    ``com.kdt.livecontainer.A7VT6RU6XK.A7VT6RU6XK``), so the plain id never
    matches HouseArrest. Look the app up via the installation proxy instead
    of guessing: Homepod/com.apple DeviceFamily names are avoided by only
    matching bundles whose prefix is the LiveContainer base id."""
    try:
        from pymobiledevice3.services.installation_proxy import InstallationProxyService
    except ImportError:
        return None

    try:
        ip = InstallationProxyService(provider)
        apps = await ip.lookup()
    except Exception as exc:
        print(f"  could not list apps to resolve bundle ({exc}); falling back to requested id")
        return None

    for bid in (apps or {}):
        if bid == BUNDLE_DEFAULT or bid.startswith(BUNDLE_DEFAULT + "."):
            return bid
    return None


async def push_file(endpoint_file, udid, file_path: str, bundle: str):
    if not os.path.isfile(file_path):
        sys.exit(f"error: source file '{file_path}' does not exist")

    filename = os.path.basename(file_path)
    file_size = os.path.getsize(file_path)

    try:
        from pymobiledevice3.services.house_arrest import HouseArrestService
    except ImportError:
        sys.exit("error: pymobiledevice3 is required (install via pip)")

    if endpoint_file:
        print(f"Connecting to device via RSD tunnel {endpoint_file}...")
        provider = await _rsd_provider(endpoint_file)
    else:
        print(f"Connecting to device {udid} via USB...")
        provider = await _usbmux_provider(udid)

    resolved = await _installed_livecontainer_bundle(provider)
    target = resolved or bundle
    print(f"Opening HouseArrest for bundle '{target}'...")

    async with await HouseArrestService.create(provider, target, documents_only=False) as ha:
        try:
            listing = await ha.listdir("/Documents")
        except Exception:
            await ha.mkdir("/Documents")
            listing = []

        remote_path = f"/Documents/{filename}"
        print(f"Transferring {filename} ({file_size / (1024*1024):.2f} MB) to {remote_path}...")

        chunk_size = 64 * 1024
        handle = await ha.fopen(remote_path, "wb")
        with open(file_path, "rb") as f:
            while True:
                chunk = f.read(chunk_size)
                if not chunk:
                    break
                await ha.fwrite(handle, chunk)
        await ha.fclose(handle)

        print(f"Successfully transferred {filename} to LiveContainer on device!")


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", help="Target device UDID (USB mode)")
    parser.add_argument("--file", required=True, help="Path to guest IPA or certificate p12 file")
    parser.add_argument("--bundle", default=BUNDLE_DEFAULT, help="LiveContainer bundle ID")
    parser.add_argument("--endpoint-file", help="RSD endpoint file (HOST PORT) for wireless mode")
    args = parser.parse_args()

    if not args.endpoint_file and not args.udid:
        sys.exit("error: either --udid (USB) or --endpoint-file (wireless RSD) is required")

    await push_file(args.endpoint_file, args.udid, args.file, args.bundle)


if __name__ == "__main__":
    asyncio.run(main())