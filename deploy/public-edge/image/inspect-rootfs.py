#!/usr/bin/env python3
"""Inspect an exported scratch image rootfs and emit a bounded report."""

from __future__ import annotations

import argparse
import json
import struct
import tarfile


ALLOWED_REGULAR = {
    ".dockerenv",
    "djonehub-edge",
    "djonehub-edge-healthcheck",
    "etc/hostname",
    "etc/hosts",
    "etc/resolv.conf",
    "etc/ssl/certs/ca-certificates.crt",
}
FORBIDDEN_BYTES = (
    b"-----BEGIN PRIVATE KEY-----",
    b"CF-Access-Client-Secret",
    b"TUNNEL_TOKEN=",
    b"TURN_AUTH_SECRET",
)


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def static_amd64_elf(data: bytes) -> bool:
    if len(data) < 64 or data[:4] != b"\x7fELF" or data[4:6] != b"\x02\x01":
        return False
    if struct.unpack_from("<H", data, 18)[0] != 62:
        return False
    offset = struct.unpack_from("<Q", data, 32)[0]
    entry_size = struct.unpack_from("<H", data, 54)[0]
    count = struct.unpack_from("<H", data, 56)[0]
    if entry_size < 56 or offset + entry_size * count > len(data):
        return False
    return all(struct.unpack_from("<I", data, offset + entry_size * index)[0] != 3 for index in range(count))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--archive", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    regular: dict[str, bytes] = {}
    with tarfile.open(args.archive, "r:*") as archive:
        for member in archive.getmembers():
            name = member.name.removeprefix("./").rstrip("/")
            if not name or member.isdir():
                continue
            if name.startswith("/") or ".." in name.split("/"):
                fail("rootfs archive contains path traversal")
            if member.issym() or member.islnk():
                fail(f"runtime rootfs contains an unapproved link: {name}")
            if not member.isfile() or name not in ALLOWED_REGULAR:
                fail(f"runtime rootfs contains an unapproved file: {name}")
            handle = archive.extractfile(member)
            if handle is None:
                fail(f"cannot read rootfs member: {name}")
            regular[name] = handle.read()
    required = {"djonehub-edge", "djonehub-edge-healthcheck", "etc/ssl/certs/ca-certificates.crt"}
    if not required.issubset(regular):
        fail("runtime rootfs is missing a required binary or CA bundle")
    all_bytes = b"".join(regular.values())
    report = {
        "architecture": "amd64",
        "ca_bundle": len(regular["etc/ssl/certs/ca-certificates.crt"]) > 100_000,
        "edge_static_elf": static_amd64_elf(regular["djonehub-edge"]),
        "healthcheck_static_elf": static_amd64_elf(regular["djonehub-edge-healthcheck"]),
        "no_credentials": not any(value in all_bytes for value in FORBIDDEN_BYTES),
        "no_shell": not any(name in regular for name in ("bin/sh", "bin/bash", "usr/bin/sh")),
        "no_source": not any(name.endswith((".go", ".c", ".h")) for name in regular),
    }
    if not all(value is True for key, value in report.items() if key != "architecture"):
        fail("runtime rootfs failed a static content gate")
    with open(args.output, "w", encoding="ascii") as handle:
        json.dump(report, handle, sort_keys=True, separators=(",", ":"))
        handle.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
