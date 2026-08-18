#!/usr/bin/env python3
"""Fail-closed policy check for Docker Compose's normalized JSON model."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


PROJECT = "dji4g-public-edge"
ROOT = "/srv/dji4g-public-edge-policy"
UID_GID = "65532:65532"
EDGE_IMAGE = (
    "ghcr.io/example/maccellular-edge@sha256:"
    + "1" * 64
)
CLOUDFLARED_IMAGE = "cloudflare/cloudflared@sha256:" + "2" * 64
COTURN_IMAGE = "coturn/coturn@sha256:" + "3" * 64
GATEWAY_ID = "policy-gateway"
ACCESS_TEAM_DOMAIN = "policy.cloudflareaccess.com"
ACCESS_AUDIENCE = "policy_audience"


def volume(source: str, target: str, *, read_only: bool = False) -> dict[str, Any]:
    result: dict[str, Any] = {
        "type": "bind",
        "source": source,
        "target": target,
        # Compose 2.40.3 preserves the explicit bind mode but omits the false
        # create_host_path value from JSON. verify-config.sh separately requires
        # the exact source-level false setting on all six normalized binds.
        "bind": {},
    }
    if read_only:
        result["read_only"] = True
    return result


def published_port(name: str, target: int, published: int, protocol: str) -> dict[str, Any]:
    return {
        "name": name,
        "mode": "ingress",
        "host_ip": "0.0.0.0",
        "target": target,
        "published": str(published),
        "protocol": protocol,
    }


def expected_model() -> dict[str, Any]:
    logging = {
        "driver": "json-file",
        "options": {"max-file": "3", "max-size": "10m"},
    }
    cloudflared = {
        "profiles": ["edge"],
        "cap_drop": ["ALL"],
        "cgroup": "private",
        "cpus": 0.5,
        "command": [
            "tunnel",
            "--no-autoupdate",
            "--metrics",
            "127.0.0.1:2000",
            "run",
            "--token-file",
            "/run/secrets/cloudflare-tunnel-token",
        ],
        "depends_on": {
            "edge": {"condition": "service_healthy", "required": True},
        },
        "entrypoint": None,
        "image": CLOUDFLARED_IMAGE,
        "healthcheck": {
            "test": ["CMD", "cloudflared", "tunnel", "--metrics", "127.0.0.1:2000", "ready"],
            "interval": "10s",
            "timeout": "5s",
            "start_period": "10s",
            "retries": 6,
        },
        "init": True,
        "ipc": "private",
        "logging": logging,
        "mem_limit": "134217728",
        "memswap_limit": "134217728",
        "networks": {"public-edge-control": None},
        "pids_limit": 64,
        "pull_policy": "never",
        "read_only": True,
        "restart": "no",
        "runtime": "runc",
        "security_opt": [
            "no-new-privileges:true",
            "apparmor=docker-default",
            "seccomp=builtin",
        ],
        "shm_size": "67108864",
        "tmpfs": ["/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"],
        "user": UID_GID,
        "volumes": [
            volume(
                f"{ROOT}/secrets/cloudflare-tunnel.token",
                "/run/secrets/cloudflare-tunnel-token",
                read_only=True,
            )
        ],
    }
    coturn_ports = [
        published_port("turn-tcp", 3478, 3478, "tcp"),
        published_port("turn-udp", 3478, 3478, "udp"),
        published_port("turn-tls", 5349, 443, "tcp"),
    ]
    coturn_ports.extend(
        published_port(f"turn-relay-{port}-udp", port, port, "udp")
        for port in range(49160, 49168)
    )
    coturn = {
        "profiles": ["turn"],
        "cap_drop": ["ALL"],
        "cgroup": "private",
        "cpus": 1,
        "command": ["-c", "/etc/coturn/turnserver.conf"],
        "entrypoint": None,
        "image": COTURN_IMAGE,
        "init": True,
        "ipc": "private",
        "logging": logging,
        "mem_limit": "268435456",
        "memswap_limit": "268435456",
        "networks": {
            "public-edge-turn": {"ipv4_address": "172.30.247.2"},
        },
        "pids_limit": 128,
        "ports": coturn_ports,
        "pull_policy": "never",
        "read_only": True,
        "restart": "no",
        "runtime": "runc",
        "security_opt": [
            "no-new-privileges:true",
            "apparmor=docker-default",
            "seccomp=builtin",
        ],
        "shm_size": "67108864",
        "tmpfs": ["/tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777"],
        "ulimits": {"nofile": {"soft": 65536, "hard": 65536}},
        "user": UID_GID,
        "volumes": [
            volume(
                f"{ROOT}/config/turnserver.conf",
                "/etc/coturn/turnserver.conf",
                read_only=True,
            ),
            volume(
                f"{ROOT}/secrets/turn-tls-cert.pem",
                "/run/secrets/turn-tls-cert.pem",
                read_only=True,
            ),
            volume(
                f"{ROOT}/secrets/turn-tls-key.pem",
                "/run/secrets/turn-tls-key.pem",
                read_only=True,
            ),
        ],
    }
    edge = {
        "profiles": ["edge"],
        "cap_drop": ["ALL"],
        "cgroup": "private",
        "cpus": 1,
        "command": None,
        "entrypoint": None,
        "environment": {
            "DJI4G_ACCESS_ALLOWED_EMAILS_FILE": "/run/config/access-allowed-emails",
            "DJI4G_ACCESS_AUDIENCE": ACCESS_AUDIENCE,
            "DJI4G_ACCESS_TEAM_DOMAIN": ACCESS_TEAM_DOMAIN,
            "DJI4G_EDGE_ENROLLMENT_ENABLED": "false",
            "DJI4G_EDGE_GATEWAY_ID": GATEWAY_ID,
            "DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE": "/run/config/gateway-public-key.pem",
            "DJI4G_EDGE_LISTEN_ADDR": "0.0.0.0:8080",
            "DJI4G_EDGE_MUTATIONS_ENABLED": "false",
            "DJI4G_EDGE_PUBLIC_HOST": "phone.example.com",
            "DJI4G_EDGE_PUSH_ENABLED": "false",
            "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY": "false",
            "DJI4G_EDGE_TURN_ISSUANCE_ENABLED": "false",
        },
        "expose": ["8080"],
        "image": EDGE_IMAGE,
        "healthcheck": {
            "test": ["CMD", "/djonehub-edge-healthcheck"],
            "interval": "10s",
            "timeout": "3s",
            "start_period": "5s",
            "retries": 3,
        },
        "init": True,
        "ipc": "private",
        "logging": logging,
        "mem_limit": "268435456",
        "memswap_limit": "268435456",
        "networks": {"public-edge-control": None},
        "pids_limit": 128,
        "pull_policy": "never",
        "read_only": True,
        "restart": "no",
        "runtime": "runc",
        "security_opt": [
            "no-new-privileges:true",
            "apparmor=docker-default",
            "seccomp=builtin",
        ],
        "shm_size": "67108864",
        "tmpfs": ["/tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777"],
        "user": UID_GID,
        "volumes": [
            volume(
                f"{ROOT}/config/gateway-public-key.pem",
                "/run/config/gateway-public-key.pem",
                read_only=True,
            ),
            volume(
                f"{ROOT}/config/access-allowed-emails",
                "/run/config/access-allowed-emails",
                read_only=True,
            ),
        ],
    }
    return {
        "name": PROJECT,
        "networks": {
            "public-edge-control": {
                "name": f"{PROJECT}_public-edge-control",
                "driver": "bridge",
                "ipam": {},
            },
            "public-edge-turn": {
                "name": f"{PROJECT}_public-edge-turn",
                "driver": "bridge",
                "ipam": {
                    "config": [
                        {
                            "subnet": "172.30.247.0/28",
                            "gateway": "172.30.247.1",
                        }
                    ]
                },
            },
        },
        "services": {
            "cloudflared": cloudflared,
            "coturn": coturn,
            "edge": edge,
        },
    }


def no_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def first_difference(expected: Any, actual: Any, path: str = "$") -> str | None:
    if type(expected) is not type(actual):
        return f"{path}: normalized type changed"
    if isinstance(expected, dict):
        expected_keys = set(expected)
        actual_keys = set(actual)
        missing = sorted(expected_keys - actual_keys)
        extra = sorted(actual_keys - expected_keys)
        if missing:
            return f"{path}: missing key {missing[0]}"
        if extra:
            return f"{path}: unapproved key {extra[0]}"
        for key in sorted(expected):
            difference = first_difference(expected[key], actual[key], f"{path}.{key}")
            if difference:
                return difference
        return None
    if isinstance(expected, list):
        if len(expected) != len(actual):
            return f"{path}: list length changed"
        for index, (expected_item, actual_item) in enumerate(zip(expected, actual)):
            difference = first_difference(expected_item, actual_item, f"{path}[{index}]")
            if difference:
                return difference
        return None
    if expected != actual:
        return f"{path}: normalized value changed"
    return None


def main() -> int:
    global ROOT, UID_GID, EDGE_IMAGE, CLOUDFLARED_IMAGE, COTURN_IMAGE
    global GATEWAY_ID, ACCESS_TEAM_DOMAIN, ACCESS_AUDIENCE
    parser = argparse.ArgumentParser()
    parser.add_argument("--default-off", action="store_true")
    parser.add_argument("--root", default=ROOT)
    parser.add_argument("--uid-gid", default=UID_GID)
    parser.add_argument("--edge-image", default=EDGE_IMAGE)
    parser.add_argument("--cloudflared-image", default=CLOUDFLARED_IMAGE)
    parser.add_argument("--coturn-image", default=COTURN_IMAGE)
    parser.add_argument("--gateway-id", default=GATEWAY_ID)
    parser.add_argument("--access-team-domain", default=ACCESS_TEAM_DOMAIN)
    parser.add_argument("--access-audience", default=ACCESS_AUDIENCE)
    parser.add_argument("json_file", type=Path)
    args = parser.parse_args()
    ROOT = args.root
    UID_GID = args.uid_gid
    EDGE_IMAGE = args.edge_image
    CLOUDFLARED_IMAGE = args.cloudflared_image
    COTURN_IMAGE = args.coturn_image
    GATEWAY_ID = args.gateway_id
    ACCESS_TEAM_DOMAIN = args.access_team_domain
    ACCESS_AUDIENCE = args.access_audience
    try:
        with args.json_file.open("r", encoding="utf-8") as handle:
            actual = json.load(handle, object_pairs_hook=no_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
        print(f"ERROR: invalid Docker Compose JSON model: {exc}", file=sys.stderr)
        return 1

    expected = (
        {"name": PROJECT, "services": {}}
        if args.default_off
        else expected_model()
    )
    difference = first_difference(expected, actual)
    if difference:
        print(f"ERROR: Docker Compose policy mismatch at {difference}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
