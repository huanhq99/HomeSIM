#!/usr/bin/env python3
"""Canonicalize Docker29 OCI export metadata without changing target blobs."""

from __future__ import annotations

import argparse
import io
import json
import os
import tarfile
from pathlib import Path, PurePosixPath


MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
MAX_MEMBERS = 10000


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def pairs_no_duplicates(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def safe_name(name: str) -> bool:
    if not name or "\\" in name or "\x00" in name:
        return False
    path = PurePosixPath(name)
    return not path.is_absolute() and all(part not in ("", ".", "..") for part in path.parts)


def canonical_json(value: object) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode("ascii")


def transform(name: str, data: bytes) -> bytes:
    if name not in {"index.json", "manifest.json"}:
        return data
    try:
        value = json.loads(data, object_pairs_hook=pairs_no_duplicates)
    except (UnicodeError, json.JSONDecodeError) as exc:
        fail(f"invalid {name}: {exc}")
    if name == "index.json":
        if not isinstance(value, dict) or not isinstance(value.get("manifests"), list):
            fail("OCI index schema is invalid during archive sanitization")
        value.pop("annotations", None)
        for descriptor in value["manifests"]:
            if not isinstance(descriptor, dict):
                fail("OCI target descriptor is invalid during archive sanitization")
            # Docker29 may copy distribution-source labels here. They are not
            # part of the content-addressed target and are deliberately removed
            # together with every importer-visible image/ref name.
            descriptor.pop("annotations", None)
    else:
        if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
            fail("Docker compatibility manifest is invalid during archive sanitization")
        value[0]["RepoTags"] = None
        value[0].pop("LayerSources", None)
    return canonical_json(value)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--epoch", required=True, type=int)
    args = parser.parse_args()
    if args.epoch < 1 or args.input.is_symlink() or not args.input.is_file():
        fail("input archive/epoch is invalid")
    if args.output.exists() or args.output.is_symlink():
        fail("output archive must not already exist")
    try:
        if args.input.stat().st_size < 1 or args.input.stat().st_size > MAX_ARCHIVE_BYTES:
            fail("input archive size is invalid")
        with tarfile.open(args.input, "r:") as source, tarfile.open(
            args.output, "x:", format=tarfile.USTAR_FORMAT
        ) as destination:
            members = source.getmembers()
            if not members or len(members) > MAX_MEMBERS:
                fail("input archive member count is invalid")
            seen: set[str] = set()
            total = 0
            for member in members:
                if not safe_name(member.name) or member.name in seen:
                    fail("input archive contains an unsafe/duplicate member")
                seen.add(member.name)
                if not (member.isfile() or member.isdir()):
                    fail("input archive contains a link/device")
                total += max(member.size, 0)
                if total > MAX_ARCHIVE_BYTES:
                    fail("input archive declared content is unbounded")
                info = tarfile.TarInfo(member.name)
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mtime = args.epoch
                if member.isdir():
                    info.type = tarfile.DIRTYPE
                    info.mode = 0o755
                    info.size = 0
                    destination.addfile(info)
                    continue
                handle = source.extractfile(member)
                if handle is None:
                    fail(f"cannot read archive member: {member.name}")
                if member.name in {"index.json", "manifest.json"}:
                    if member.size > 4 * 1024 * 1024:
                        fail(f"archive metadata is unbounded: {member.name}")
                    data = handle.read(member.size + 1)
                    if len(data) != member.size:
                        fail(f"archive member size changed: {member.name}")
                    data = transform(member.name, data)
                    stream = io.BytesIO(data)
                    info.size = len(data)
                else:
                    stream = handle
                    info.size = member.size
                info.mode = 0o600
                destination.addfile(info, stream)
        os.chmod(args.output, 0o600)
    except (OSError, tarfile.TarError) as exc:
        fail(f"cannot sanitize Docker archive: {exc}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
