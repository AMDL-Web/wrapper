#!/usr/bin/env python3
"""Slim a built wrapper-lite rootfs to the libraries wrapper-lite needs.

rootfs/system/lib64 holds 98 libraries from the Apple Music Android app and
AOSP. wrapper-lite uses a small part of them; the rest is pulled in only by
DT_NEEDED entries of the Apple libraries (the UI, graphics, audio and media
frameworks), and none of that code runs in a headless service.

This script keeps KEEP and removes every other library. A kept library that
lists a removed one in DT_NEEDED would no longer load, so each such entry is
pointed at the string of another entry of the same library (libc.so, which
every library lists); the loader sees one name twice and loads it once. No
kept library imports a symbol that only a removed library defines (checked
with readelf --dyn-syms on the full 98), so no call resolves into a removed
library. `--report` prints each entry it rewrites.

Usage: slim-rootfs.py ROOTFS [--report]
ROOTFS is edited in place; run it on a copy.
"""
import os
import struct
import sys

# Libraries wrapper-lite loads: the closure of the lite binary's DT_NEEDED
# entries (main() checks that every name here is in it). libCoreFP.so and
# libCoreLSKD.so hold the FairPlay code (lite/playback.cpp hooks into
# libCoreLSKD.so by address). libCoreADI.so is the one a DT_NEEDED walk
# would not justify: nothing imports from it, but libCoreFP.so and
# libstoreservicescore.so look up its exports (cvu8io98wun, vdfut768ig) by
# dlsym. Without it every request fails to build its anisette headers
# (ADISetProvisioningPath -45075) and /m3u8 and the token refresh fail.
KEEP = frozenset("""
libandroidappmusic.so libstoreservicescore.so libmediaplatform.so
libc++_shared.so libcurl.so liblog.so libc.so libm.so libstdc++.so
libCoreFoundation.so libCoreFP.so libCoreLSKD.so libCoreADI.so libdaapkit.so
libmedialibrarycore.so libdispatch.so libBlocksRuntime.so
libicudata_sv_apple.so libicui18n_sv_apple.so libicuuc_sv_apple.so
libxml2.so libz.so
""".split())

DT_NULL, DT_NEEDED = 0, 1
PT_LOAD, PT_DYNAMIC = 1, 2


class Elf:
    def __init__(self, path):
        self.path = path
        self.data = bytearray(open(path, "rb").read())
        d = self.data
        if d[:4] != b"\x7fELF" or d[4] != 2 or d[5] != 1:
            raise SystemExit(f"{path}: not a little-endian ELF64")
        phoff = struct.unpack_from("<Q", d, 32)[0]
        phentsize, phnum = struct.unpack_from("<HH", d, 54)
        loads, dyn = [], None
        for i in range(phnum):
            o = phoff + i * phentsize
            ptype = struct.unpack_from("<I", d, o)[0]
            off, vaddr, _, filesz = struct.unpack_from("<QQQQ", d, o + 8)
            if ptype == PT_LOAD:
                loads.append((vaddr, off, filesz))
            elif ptype == PT_DYNAMIC:
                dyn = off
        if dyn is None:
            raise SystemExit(f"{path}: no PT_DYNAMIC")
        self.entries = []  # (file offset of the entry, tag, value)
        strtab = None
        o = dyn
        while True:
            tag, val = struct.unpack_from("<qQ", d, o)
            self.entries.append((o, tag, val))
            if tag == 5:
                strtab = val
            if tag == DT_NULL:
                break
            o += 16
        if strtab is None:
            raise SystemExit(f"{path}: no DT_STRTAB")
        for vaddr, off, filesz in loads:
            if vaddr <= strtab < vaddr + filesz:
                self.strtab = off + strtab - vaddr
                break
        else:
            raise SystemExit(f"{path}: DT_STRTAB is outside every PT_LOAD")

    def _name(self, val):
        s = self.strtab + val
        return self.data[s:self.data.index(b"\0", s)].decode()

    def needed(self):
        """DT_NEEDED entries as (entry offset, name)."""
        return [(o, self._name(v)) for o, t, v in self.entries if t == DT_NEEDED]

    def point_needed_at(self, entry, other):
        """Make DT_NEEDED entry name the same string as entry `other`."""
        val = next(v for o, t, v in self.entries if o == other)
        struct.pack_into("<Q", self.data, entry + 8, val)

    def save(self):
        mode = os.stat(self.path).st_mode
        tmp = self.path + ".slim"
        with open(tmp, "wb") as f:
            f.write(self.data)
        os.chmod(tmp, mode)
        os.replace(tmp, self.path)


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    report = "--report" in sys.argv
    if len(args) != 1:
        raise SystemExit(__doc__)
    root = args[0]
    lib = os.path.join(root, "system", "lib64")
    present = {n for n in os.listdir(lib) if n.endswith(".so")}
    missing = KEEP - present
    if missing:
        raise SystemExit(f"KEEP names libraries the rootfs does not have: {sorted(missing)}")
    removed = sorted(present - KEEP)

    targets = [os.path.join(root, "system", "bin", "lite")] + [os.path.join(lib, n) for n in sorted(KEEP)]
    cut = 0
    for path in targets:
        elf = Elf(path)
        needed = elf.needed()
        # The loader, not a file in lib64, supplies names such as libdl.so.
        keepers = [(o, n) for o, n in needed if n in KEEP or n not in present]
        if not keepers:
            raise SystemExit(f"{path}: lists no kept library to stand in for a removed one")
        stand_in = next((o for o, n in keepers if n == "libc.so"), keepers[0][0])
        changed = False
        for o, n in needed:
            if n in present and n not in KEEP:
                elf.point_needed_at(o, stand_in)
                changed = True
                cut += 1
                if report:
                    print(f"cut {os.path.basename(path)} -> {n}")
        if changed:
            elf.save()

    # Everything a kept library lists must now be kept or provided by the
    # loader, and every kept library must be reachable from lite.
    reached, todo = set(), [targets[0]]
    while todo:
        for _, n in Elf(todo.pop()).needed():
            if n in present and n not in KEEP:
                raise SystemExit(f"{n} is still listed after the cuts")
            if n in KEEP and n not in reached:
                reached.add(n)
                todo.append(os.path.join(lib, n))
    if reached != KEEP:
        raise SystemExit(f"KEEP names libraries lite does not reach: {sorted(KEEP - reached)}")

    freed = 0
    for n in removed:
        p = os.path.join(lib, n)
        freed += os.path.getsize(p)
        os.unlink(p)
    print(f"kept {len(KEEP)} libraries, removed {len(removed)} ({freed / 1048576:.1f} MiB), "
          f"cut {cut} dependency entries")


if __name__ == "__main__":
    main()
