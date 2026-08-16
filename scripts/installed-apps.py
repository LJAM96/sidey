#!/usr/bin/env python3
"""
installed-apps.py: Inventory the apps on a device for the Sidey dashboard.

Reports two groups:
  - system apps: via InstallationProxyService (all User apps installed by iOS)
  - LiveContainer guests: the app directories inside the LiveContainer
    container's Documents/Applications, plus stray files in Documents

Output is a single JSON document on stdout:
  {"system": [{"bundle_id","name","version"}...],
   "guests": [{"bundle_id","name"}...],
   "documents": ["filename", ...],
   "livecontainer_bundle": "com.kdt.livecontainer.A7VT6RU6XK.A7VT6RU6XK"}

Usage:
  python3 installed-apps.py --endpoint-file /run/sidey/rsd-endpoint [--bundle com.kdt.livecontainer]
  python3 installed-apps.py --udid <UDID> [--bundle com.kdt.livecontainer]
"""

import argparse
import asyncio
import json
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

    isideload shadows the host bundle id by appending the team id (often
    twice), so the plain id never matches HouseArrest. Look it up via the
    installation proxy instead."""
    try:
        from pymobiledevice3.services.installation_proxy import InstallationProxyService
    except ImportError:
        return None

    try:
        ip = InstallationProxyService(provider)
        apps = await ip.lookup()
    except Exception:
        return None

    for bid in (apps or {}):
        if bid == BUNDLE_DEFAULT or bid.startswith(BUNDLE_DEFAULT + "."):
            return bid
    return None


async def _guest_app_names(provider, target: str):
    """Best-effort display names for LiveContainer guest apps.

    Each guest lives in /Documents/Applications/<something>/ where the guest
    directory itself is the .app bundle, so Info.plist sits at its root (some
    installs nest one .app subdirectory instead). Read Info.plist over
    HouseArrest to find the display name and version."""
    import plistlib

    from pymobiledevice3.services.house_arrest import HouseArrestService

    names = {}
    try:
        async with await HouseArrestService.create(provider, target, documents_only=False) as ha:
            apps_dir = await ha.listdir("/Documents/Applications")
            for item in apps_dir:
                guest = str(item)
                if guest in (".", ".."):
                    continue
                names[guest] = {"name": guest, "version": "", "bundle_id": guest}

                plist_candidates = [f"/Documents/Applications/{guest}/Info.plist"]
                try:
                    entries = await ha.listdir(f"/Documents/Applications/{guest}")
                    for entry in sorted(entries):
                        entry_name = str(entry)
                        if entry_name.endswith(".app"):
                            plist_candidates.append(
                                f"/Documents/Applications/{guest}/{entry_name}/Info.plist")
                except Exception:
                    pass

                for candidate in plist_candidates:
                    try:
                        plist_data = await ha.get_file_contents(candidate)
                        parsed = plistlib.loads(plist_data)
                        names[guest] = {
                            "name": parsed.get("CFBundleDisplayName")
                                    or parsed.get("CFBundleName")
                                    or guest,
                            "version": parsed.get("CFBundleShortVersionString", ""),
                            "bundle_id": parsed.get("CFBundleIdentifier", guest),
                        }
                        break
                    except Exception:
                        continue
    except Exception:
        pass
    return names


async def inventory(endpoint_file, udid, bundle: str):
    from pymobiledevice3.services.house_arrest import HouseArrestService
    from pymobiledevice3.services.installation_proxy import InstallationProxyService

    if endpoint_file:
        provider = await _rsd_provider(endpoint_file)
    else:
        provider = await _usbmux_provider(udid)

    result = {
        "system": [],
        "guests": [],
        "documents": [],
        "livecontainer_bundle": None,
    }

    resolved = await _installed_livecontainer_bundle(provider)
    result["livecontainer_bundle"] = resolved
    target = resolved or bundle

    try:
        ip = InstallationProxyService(provider)
        apps = await ip.get_apps(application_type="User")
        for bid, meta in sorted((apps or {}).items()):
            result["system"].append({
                "bundle_id": bid,
                "name": meta.get("CFBundleDisplayName")
                        or meta.get("CFBundleName") or bid,
                "version": meta.get("CFBundleShortVersionString", ""),
            })
    except Exception as exc:
        result["system_error"] = str(exc)

    try:
        async with await HouseArrestService.create(provider, target, documents_only=False) as ha:
            try:
                documents = await ha.listdir("/Documents")
            except Exception:
                await ha.mkdir("/Documents")
                documents = []
            for item in documents:
                name = str(item)
                if name in ("Applications", "Data", "Tweaks"):
                    continue
                result["documents"].append(name)

            try:
                apps_dir = await ha.listdir("/Documents/Applications")
            except Exception:
                apps_dir = []
            guest_names = await _guest_app_names(provider, target)
            for item in sorted(apps_dir):
                guest_name = str(item)
                if guest_name in (".", ".."):
                    continue
                meta = guest_names.get(guest_name, {})
                result["guests"].append({
                    "bundle_id": meta.get("bundle_id") or guest_name,
                    "name": meta.get("name") or guest_name,
                    "version": meta.get("version", ""),
                    "container": guest_name,
                })
    except Exception as exc:
        result["guest_error"] = str(exc)

    print(json.dumps(result))


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--udid", help="Target device UDID (USB mode)")
    parser.add_argument("--bundle", default=BUNDLE_DEFAULT, help="LiveContainer bundle ID")
    parser.add_argument("--endpoint-file", help="RSD endpoint file (HOST PORT) for wireless mode")
    args = parser.parse_args()

    if not args.endpoint_file and not args.udid:
        sys.exit("error: either --udid (USB) or --endpoint-file (wireless RSD) is required")

    await inventory(args.endpoint_file, args.udid, args.bundle)


if __name__ == "__main__":
    asyncio.run(main())