#!/usr/bin/env python3
"""Atomically rename one staged directory to an absent sibling path."""

from __future__ import annotations

import argparse
import ctypes
import errno
import os
import platform
import stat
from pathlib import Path


AT_FDCWD = -100
RENAME_NOREPLACE = 1
RENAME_EXCL = 0x00000004


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def canonical_parent(path: Path) -> tuple[Path, str]:
    if not path.is_absolute() or path.name in ("", ".", ".."):
        fail("source and target must be clean absolute directory paths")
    parent = path.parent
    try:
        physical_parent = parent.resolve(strict=True)
    except OSError as exc:
        fail(f"cannot resolve parent directory: {exc}")
    if parent != physical_parent or not physical_parent.is_dir():
        fail("source and target parents must be physical canonical directories")
    return physical_parent, path.name


def rename_noreplace(source: bytes, target: bytes) -> None:
    libc = ctypes.CDLL(None, use_errno=True)
    system = platform.system()
    if system == "Linux":
        function = getattr(libc, "renameat2", None)
        if function is None:
            fail("renameat2(RENAME_NOREPLACE) is unavailable")
        function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(AT_FDCWD, source, AT_FDCWD, target, RENAME_NOREPLACE)
    elif system == "Darwin":
        function = getattr(libc, "renamex_np", None)
        if function is None:
            fail("renamex_np(RENAME_EXCL) is unavailable")
        function.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(source, target, RENAME_EXCL)
    else:
        fail(f"unsupported host for atomic no-replace commit: {system}")
    if result != 0:
        value = ctypes.get_errno()
        if value in (errno.EEXIST, errno.ENOTEMPTY):
            fail("output path appeared before atomic commit")
        raise OSError(value, os.strerror(value))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--target", type=Path, required=True)
    args = parser.parse_args()
    source_parent, source_name = canonical_parent(args.source)
    target_parent, target_name = canonical_parent(args.target)
    if source_parent != target_parent:
        fail("atomic commit requires source and target to be siblings")
    source = source_parent / source_name
    target = target_parent / target_name
    try:
        source_stat = source.lstat()
    except OSError as exc:
        fail(f"staged output is missing: {exc}")
    if not stat.S_ISDIR(source_stat.st_mode) or source.is_symlink():
        fail("staged output must be a real directory")
    if target.exists() or target.is_symlink():
        fail("output already exists")
    rename_noreplace(os.fsencode(source), os.fsencode(target))
    committed_stat = target.lstat()
    if not stat.S_ISDIR(committed_stat.st_mode) or (
        committed_stat.st_dev, committed_stat.st_ino
    ) != (source_stat.st_dev, source_stat.st_ino):
        fail("atomic commit inode verification failed")
    print(f"{committed_stat.st_dev}:{committed_stat.st_ino}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
