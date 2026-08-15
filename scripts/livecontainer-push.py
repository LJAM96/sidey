#!/usr/bin/env python3
"""
livecontainer-push.py: Push a raw guest IPA directly into LiveContainer's container
on an iOS device using pymobiledevice3 HouseArrestService over USB or wireless RSD tunnel.

Usage:
  python3 livecontainer-push.py --udid <UDID> --ipa /path/to/app.ipa [--bundle com.kdt.livecontainer]
"""

import argparse
import asyncio
import os
import sys

BUNDLE_DEFAULT = "com.kdt.livecontainer"


async def push_ipa(udid: str, ipa_path: str, bundle: str):
    if not os.path.isfile(ipa_path):
        sys.exit(f"error: source ipa file '{ipa_path}' does not exist")

    filename = os.path.basename(ipa_path)
    file_size = os.path.getsize(ipa_path)

    try:
        from pymobiledevice3.lockdown import create_using_usbmux
        from pymobiledevice3.services.house_arrest import HouseArrestService
    except ImportError:
        sys.exit("error: pymobiledevice3 is required (install via pip)")

    print(f"Connecting to device {udid}...")
    lockdown = await create_using_usbmux(udid)
    print(f"Opening HouseArrest for bundle '{bundle}'...")
    
    async with await HouseArrestService.create(lockdown, bundle, documents_only=False) as ha:
        # Check / create Documents directory
        try:
            listing = await ha.listdir("/Documents")
        except Exception:
            await ha.mkdir("/Documents")
            listing = []

        remote_path = f"/Documents/{filename}"
        print(f"Transferring {filename} ({file_size / (1024*1024):.2f} MB) to {remote_path}...")

        chunk_size = 64 * 1024
        handle = await ha.fopen(remote_path, "wb")
        with open(ipa_path, "rb") as f:
            while True:
                chunk = f.read(chunk_size)
                if not chunk:
                    break
                await ha.fwrite(handle, chunk)
        await ha.fclose(handle)

        print(f"Successfully transferred {filename} to LiveContainer on device {udid}!")


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", required=True, help="Target device UDID")
    parser.add_argument("--ipa", required=True, help="Path to guest IPA file")
    parser.add_argument("--bundle", default=BUNDLE_DEFAULT, help="LiveContainer bundle ID")
    args = parser.parse_args()

    await push_ipa(args.udid, args.ipa, args.bundle)


if __name__ == "__main__":
    asyncio.run(main())
