#!/usr/bin/env python3
"""Place a pairing file into an installed app's Documents directory (SideStore et al).

Uses the house_arrest service, exactly like iloader's "Place" button: the
pairing record is written to the app container at
<path> (default ALTPairingFile.mobiledevicepairing) so the app finds it on
launch and never shows the file picker.

Usage:
  place-pairing-file.py [--udid UDID] [--bundle BUNDLE_ID]
                        [--path PATH] [--pairing-file FILE]

Options:
  --udid UDID          Device UDID (default: first device on usbmuxd)
  --bundle BUNDLE_ID   App bundle id (default: com.SideStore.SideStore.A7VT6RU6XK)
  --path PATH          Remote path under the container (default: ALTPairingFile.mobiledevicepairing)
  --pairing-file FILE  Pairing file to place (default: <UDID>.mobiledevicepairing
                       regenerated from the usbmuxd lockdown record)

Notes:
  - Requires pymobiledevice3 (e.g. /opt/sidey/venv-pmd3/bin/python3).
  - VendDocuments is denied on modern iOS (17+); VendContainer is used instead
    and the file is written under /Documents/.
  - The device must be paired with this host (usbmuxd record present).
"""

import argparse
import asyncio
import plistlib
import sys

BUNDLE_DEFAULT = "com.SideStore.SideStore.A7VT6RU6XK"
PATH_DEFAULT = "ALTPairingFile.mobiledevicepairing"
LOCKDOWN_DIR = "/var/lib/lockdown"


async def get_udid(udid_arg):
    from pymobiledevice3.usbmux import list_devices

    devices = await list_devices()
    if not devices:
        sys.exit("error: no device connected")
    if udid_arg:
        for d in devices:
            if d.serial == udid_arg:
                return udid_arg
        sys.exit(f"error: device {udid_arg} not found on usbmuxd")
    return devices[0].serial


def pairing_file_from_record(udid):
    import os

    record_path = os.path.join(LOCKDOWN_DIR, f"{udid}.plist")
    if not os.path.exists(record_path):
        sys.exit(f"error: no lockdown record at {record_path}; pair the device first")
    with open(record_path, "rb") as f:
        record = plistlib.load(f)
    record["UDID"] = udid
    data = plistlib.dumps(record, fmt=plistlib.FMT_XML)
    print(f"regenerated pairing file from {record_path} ({len(data)} bytes)")
    return data


async def place(udid, bundle, remote_path, pairing_data):
    from pymobiledevice3.lockdown import create_using_usbmux
    from pymobiledevice3.services.house_arrest import HouseArrestService

    lockdown = await create_using_usbmux(udid)
    async with await HouseArrestService.create(lockdown, bundle, documents_only=False) as ha:
        handle = await ha.fopen(f"/Documents/{remote_path}", "w")
        await ha.fwrite(handle, pairing_data)
        await ha.fclose(handle)
        listing = await ha.listdir("/Documents")
        if remote_path.split("/")[-1] not in listing:
            sys.exit(f"error: {remote_path} missing from /Documents listing {listing}")


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", default=None)
    parser.add_argument("--bundle", default=BUNDLE_DEFAULT)
    parser.add_argument("--path", default=PATH_DEFAULT)
    parser.add_argument("--pairing-file", default=None)
    args = parser.parse_args()

    udid = await get_udid(args.udid)
    if args.pairing_file:
        with open(args.pairing_file, "rb") as f:
            pairing_data = f.read()
    else:
        pairing_data = pairing_file_from_record(udid)

    await place(udid, args.bundle, args.path, pairing_data)
    print(f"placed {len(pairing_data)} bytes at /Documents/{args.path} in {args.bundle} (udid {udid})")


if __name__ == "__main__":
    asyncio.run(main())
