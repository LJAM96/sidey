#!/usr/bin/env python3
"""Rewrite an IPA so installd accepts it on tvOS (proven Phase G recipe).

Apple TV installd rejects iOS-built archives even when the embedded
provisioning profile and UIDeviceFamily are correct. Two structural fixes are
needed (see plan.md Phase G status 2026-08-09):

  1. Info.plist: UIDeviceFamily [3], CFBundleSupportedPlatforms [TVOS],
     drop LSRequiresIPhoneOS, add the DT* platform keys.
  2. Mach-O: every executable/dylib carrying an LC_BUILD_VERSION command with
     platform 2 (iOS) is rewritten to platform 3 (tvOS) so installd sees a
     binary list like [tvOS, arm64] instead of [iOS, arm64].

Writes a new IPA (zip, DEFLATED) to OUT; the source tree is extracted to a
temporary directory first.

Usage:
  tvos-patch-ipa.py IN.ipa OUT.ipa [--minimum-os 15.0]

Note: signing must be re-done AFTER patching (plumesign sign); a profile
signed for iOS but with patched metadata is still rejected.
"""

import argparse
import shutil
import struct
import sys
import tempfile
import zipfile
from pathlib import Path

# LC_BUILD_VERSION constant + platform enum values.
LC_BUILD_VERSION = 0x32
PLATFORM_IOS = 2
PLATFORM_TVOS = 3

# Decompression-bomb guard: ipa sizes are already bounded by the upload cap,
# but `extractall` of a hostile archive could still expand far beyond the
# archive's own size. Bound the unpacked total and any single entry up front.
MAX_ENTRY_BYTES = 2 << 30  # 2 GiB per file
MAX_TOTAL_BYTES = 8 << 30  # 8 GiB unpacked archive


def guard_zip_sizes(zf: zipfile.ZipFile) -> None:
    total = 0
    for info in zf.infolist():
        if info.file_size > MAX_ENTRY_BYTES:
            raise SystemExit(
                f"refusing oversized zip entry {info.filename!r} ({info.file_size} bytes)"
            )
        total += info.file_size
    if total > MAX_TOTAL_BYTES:
        raise SystemExit(f"refusing oversized archive ({total} bytes unpacked)")


def patch_info_plist(info_plist: Path, minimum_os: str) -> None:
    """Patch UIDeviceFamily / CFBundleSupportedPlatforms and DT* keys."""
    try:
        import plistlib

        with open(info_plist, "rb") as f:
            info = plistlib.load(f)
    except Exception as e:  # noqa: BLE001 - unpack the plist or die
        raise SystemExit(f"read Info.plist failed: {e}") from e

    info["UIDeviceFamily"] = [3]
    info["CFBundleSupportedPlatforms"] = ["TVOS"]
    info.pop("LSRequiresIPhoneOS", None)
    info["MinimumOSVersion"] = minimum_os
    info["DTPlatformName"] = "tvos"
    info["DTPlatformVersion"] = minimum_os
    info["DTSDKName"] = f"tvos{minimum_os}"
    info["DTCompiler"] = "com.apple.compilers.llvm.clang.1_0"

    with open(info_plist, "wb") as f:
        import plistlib

        plistlib.dump(info, f, fmt=plistlib.FMT_BINARY)


def is_macho(path: Path) -> bool:
    """Heuristic: file begins with a Mach-O magic (thin 64-bit or fat)."""
    with open(path, "rb") as f:
        magic = f.read(4)
    return magic in (b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf", b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca")


def patch_macho_platform(path: Path) -> bool:
    """Rewrite every LC_BUILD_VERSION platform 2 (iOS) to 3 (tvOS).

    Only thin 64-bit Mach-Os are rewritten here. Fat (universal) binaries are
    left untouched - the Phase G proof signed a thin arm64 binary.
    """
    data = bytearray(path.read_bytes())
    if len(data) < 32:
        return False
    # mach_header_64: magic(4) cputype(4) cpusubtype(4) filetype(4) ncmds(4) sizeofcmds(4) flags(4) reserved(4)
    ncmds = int.from_bytes(data[16:20], "little")
    pos = 32
    patched = False
    for _ in range(ncmds):
        if pos + 8 > len(data):
            return False
        cmd = int.from_bytes(data[pos : pos + 4], "little")
        size = int.from_bytes(data[pos + 4 : pos + 8], "little")
        if size < 8 or pos + size > len(data):
            return False
        if cmd == LC_BUILD_VERSION:
            platform = int.from_bytes(data[pos + 8 : pos + 12], "little")
            if platform == PLATFORM_IOS:
                data[pos + 8 : pos + 12] = struct.pack("<I", PLATFORM_TVOS)
                patched = True
            elif platform != PLATFORM_TVOS:
                print(f"  warning: LC_BUILD_VERSION platform {platform} left as-is", file=sys.stderr)
        pos += size
    if patched:
        path.write_bytes(data)
    return patched


def find_app_dirs(stage: Path) -> list[Path]:
    """Return Payload/*.app directories."""
    payload = stage / "Payload"
    if not payload.is_dir():
        raise SystemExit("no Payload/ directory in ipa")
    return sorted(p for p in payload.iterdir() if p.is_dir() and p.name.endswith(".app"))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("in_ipa")
    parser.add_argument("out_ipa")
    parser.add_argument("--minimum-os", default="15.0")
    args = parser.parse_args()

    src = Path(args.in_ipa)
    out = Path(args.out_ipa)
    if not src.is_file():
        raise SystemExit(f"ipa not found: {src}")

    stage = Path(tempfile.mkdtemp(prefix="tvos-patch-"))
    try:
        with zipfile.ZipFile(src) as z:
            guard_zip_sizes(z)
            z.extractall(stage)

        apps = find_app_dirs(stage)
        if not apps:
            raise SystemExit("no .app bundle in ipa")
        print(f"patching {len(apps)} app bundle(s)")
        for app in apps:
            info_plist = app / "Info.plist"
            print(f"  {app.name}")
            patch_info_plist(info_plist, args.minimum_os)
            for f in app.rglob("*"):
                if not f.is_file():
                    continue
                if f.name in ("Info.plist", "embedded.mobileprovision", "PkgInfo"):
                    continue
                rel = f.relative_to(app)
                if any(p == "_CodeSignature" or p.endswith(".dSYM") for p in rel.parts):
                    continue
                if not is_macho(f):
                    continue
                if patch_macho_platform(f):
                    print(f"    mach-o platform -> tvOS: {rel}")

        if out.exists():
            out.unlink()
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
            for f in sorted(stage.rglob("*")):
                if f.is_file():
                    z.write(f, f.relative_to(stage))
        print(f"OK {out} ({out.stat().st_size} bytes)")
        return 0
    finally:
        shutil.rmtree(stage, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())