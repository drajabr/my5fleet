#!/usr/bin/env python3
"""Rename Wine-specific export symbols in PE DLLs/EXEs so that
GetProcAddress("__wine_dbg_header") etc. return NULL.

Each replacement is the same byte-length so the PE structure is preserved.
Uses mmap for fast in-place editing without reading entire files into memory.
Usage: python3 hide_wine_exports.py <directory> [directory2 ...]
"""
import glob, mmap, os, sys

SWAPS = [
    (b"__wine_dbg_header", b"__xine_dbg_header"),
    (b"__wine_dbg_strdup", b"__xine_dbg_strdup"),
    (b"__wine_dbg_output", b"__xine_dbg_output"),
    (b"wine_get_version\x00",  b"xine_get_version\x00"),
    (b"wine_get_build_id\x00", b"xine_get_build_id\x00"),
]

patched = 0
total = 0
for d in sys.argv[1:]:
    targets = glob.glob(f"{d}/**/*.dll", recursive=True) \
            + glob.glob(f"{d}/**/*.exe", recursive=True)
    total += len(targets)
    for f in targets:
        try:
            sz = os.path.getsize(f)
            if sz == 0:
                continue
            with open(f, "r+b") as fh:
                with mmap.mmap(fh.fileno(), 0) as mm:
                    hit = False
                    for old, new in SWAPS:
                        idx = 0
                        while True:
                            idx = mm.find(old, idx)
                            if idx == -1:
                                break
                            mm[idx:idx+len(old)] = new
                            idx += len(new)
                            hit = True
                    if hit:
                        patched += 1
        except Exception as exc:
            print(f"Warning: {f}: {exc}", file=sys.stderr)

print(f"Patched {patched}/{total} PE files")
