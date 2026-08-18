#!/usr/bin/env python3
"""Reject Compose project orphans and verify exact post-action state."""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
import re
import stat
from pathlib import Path


PROJECT = "dji4g-public-edge"
ALLOWED = {"edge", "cloudflared", "coturn"}
EDGE_ENV_KEYS = {
    "DJI4G_EDGE_LISTEN_ADDR", "DJI4G_EDGE_PUBLIC_HOST", "DJI4G_EDGE_GATEWAY_ID",
    "DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE", "DJI4G_ACCESS_TEAM_DOMAIN",
    "DJI4G_ACCESS_AUDIENCE", "DJI4G_ACCESS_ALLOWED_EMAILS_FILE",
    "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY", "DJI4G_EDGE_ENROLLMENT_ENABLED",
    "DJI4G_EDGE_MUTATIONS_ENABLED", "DJI4G_EDGE_PUSH_ENABLED",
    "DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
}
CLOUDFLARED_COMMAND = [
    "tunnel", "--no-autoupdate", "--metrics", "127.0.0.1:2000", "run",
    "--token-file", "/run/secrets/cloudflare-tunnel-token",
]
NETWORK_KEYS = {"control", "turn"}
NETWORK_NAMES = {
    "control": f"{PROJECT}_public-edge-control",
    "turn": f"{PROJECT}_public-edge-turn",
}
NETWORK_ALLOWED_KEYS = {
    "Name", "Id", "Created", "Scope", "Driver", "EnableIPv4", "EnableIPv6",
    "IPAM", "Internal", "Attachable", "Ingress", "ConfigFrom", "ConfigOnly",
    "Containers", "Options", "Labels", "Peers", "Services", "Status",
}
ENDPOINT_ALLOWED_KEYS = {
    "IPAMConfig", "Links", "Aliases", "MacAddress", "DriverOpts", "GwPriority",
    "NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen",
    "IPv6Gateway", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "DNSNames",
}
NETWORK_LABELS = {
    "control": {
        "com.docker.compose.network": "public-edge-control",
        "com.docker.compose.project": PROJECT,
        "com.docker.compose.version": "2.40.3",
    },
    "turn": {
        "com.docker.compose.network": "public-edge-turn",
        "com.docker.compose.project": PROJECT,
        "com.docker.compose.version": "2.40.3",
    },
}
DOCKER29_MASKED_PATHS = [
    "/proc/acpi",
    "/proc/asound",
    "/proc/interrupts",
    "/proc/kcore",
    "/proc/keys",
    "/proc/latency_stats",
    "/proc/sched_debug",
    "/proc/scsi",
    "/proc/timer_list",
    "/proc/timer_stats",
    "/sys/devices/virtual/powercap",
    "/sys/firmware",
]
DOCKER29_READONLY_PATHS = [
    "/proc/bus",
    "/proc/fs",
    "/proc/irq",
    "/proc/sys",
    "/proc/sysrq-trigger",
]


def verify_masked_paths(value: object, context: str) -> None:
    if not isinstance(value, list) or value[:len(DOCKER29_MASKED_PATHS)] != DOCKER29_MASKED_PATHS:
        fail(f"{context} Docker 29 base masked-path policy changed")
    extras = value[len(DOCKER29_MASKED_PATHS):]
    cpu_indices: list[int] = []
    for path in extras:
        match = re.fullmatch(
            r"/sys/devices/system/cpu/cpu(0|[1-9][0-9]*)/thermal_throttle",
            path if isinstance(path, str) else "",
        )
        if match is None:
            fail(f"{context} Docker 29 masked-path allowlist changed")
        cpu_indices.append(int(match.group(1)))
    if cpu_indices != sorted(set(cpu_indices)):
        fail(f"{context} Docker 29 CPU thermal masked paths are duplicated/unordered")


def expected_ports(service: str) -> tuple[dict, dict]:
    if service == "edge":
        return {}, {"8080/tcp": None}
    if service == "cloudflared":
        return {}, {}
    bindings = {
        "3478/tcp": [{"HostIp": "0.0.0.0", "HostPort": "3478"}],
        "3478/udp": [{"HostIp": "0.0.0.0", "HostPort": "3478"}],
        "5349/tcp": [{"HostIp": "0.0.0.0", "HostPort": "443"}],
    }
    for port in range(49160, 49168):
        bindings[f"{port}/udp"] = [{"HostIp": "0.0.0.0", "HostPort": str(port)}]
    return bindings, bindings


def verify_network(service: str, container: dict, host: dict) -> None:
    expected_bindings, expected_runtime_ports = expected_ports(service)
    if host.get("PortBindings") != expected_bindings:
        fail(f"service {service} host port bindings changed")
    for key, expected in {
        "PidMode": "", "IpcMode": "private", "UTSMode": "",
        "UsernsMode": "", "CgroupnsMode": "private",
    }.items():
        if key not in host or host[key] != expected or not isinstance(host[key], str):
            fail(f"service {service} namespace mode changed: {key}")
    settings = container.get("NetworkSettings")
    if not isinstance(settings, dict) or settings.get("Ports") != expected_runtime_ports:
        fail(f"service {service} resolved ports changed")
    networks = settings.get("Networks")
    expected_network = (
        f"{PROJECT}_public-edge-turn" if service == "coturn"
        else f"{PROJECT}_public-edge-control"
    )
    if not isinstance(networks, dict) or set(networks) != {expected_network}:
        fail(f"service {service} joined an unapproved/missing network")
    endpoint = networks[expected_network]
    if not isinstance(endpoint, dict) or set(endpoint) - ENDPOINT_ALLOWED_KEYS:
        fail(f"service {service} network endpoint evidence is invalid")
    valid_hex_identifier(endpoint.get("NetworkID"), f"service {service} endpoint network ID")
    valid_hex_identifier(endpoint.get("EndpointID"), f"service {service} endpoint ID")
    mac = endpoint.get("MacAddress")
    if not isinstance(mac, str) or re.fullmatch(r"[0-9a-f]{2}(?::[0-9a-f]{2}){5}", mac) is None:
        fail(f"service {service} endpoint MAC address is invalid")
    if endpoint.get("Links") not in (None, []) or endpoint.get("DriverOpts") not in (None, {}):
        fail(f"service {service} endpoint links/driver options changed")
    if endpoint.get("GwPriority") not in (None, 0) or isinstance(endpoint.get("GwPriority"), bool):
        fail(f"service {service} endpoint gateway priority changed")
    if (
        endpoint.get("IPv6Gateway") not in (None, "")
        or endpoint.get("GlobalIPv6Address") not in (None, "")
        or endpoint.get("GlobalIPv6PrefixLen") not in (None, 0)
        or isinstance(endpoint.get("GlobalIPv6PrefixLen"), bool)
    ):
        fail(f"service {service} endpoint unexpectedly has IPv6")
    ipam_config = endpoint.get("IPAMConfig")
    if service == "coturn":
        if ipam_config not in (None, {"IPv4Address": "172.30.247.2"}):
            fail("coturn endpoint requested IPAM config changed")
    elif ipam_config is not None:
        fail(f"service {service} endpoint unexpectedly has requested IPAM config")
    address = endpoint.get("IPAddress")
    gateway = endpoint.get("Gateway")
    prefix = endpoint.get("IPPrefixLen")
    try:
        parsed_address = ipaddress.IPv4Address(address)
        parsed_gateway = ipaddress.IPv4Address(gateway)
    except (ipaddress.AddressValueError, TypeError):
        fail(f"service {service} network IPv4 evidence is invalid")
    if isinstance(prefix, bool) or not isinstance(prefix, int) or not 1 <= prefix <= 32:
        fail(f"service {service} network prefix evidence is invalid")
    if any((parsed_address.is_unspecified, parsed_address.is_loopback, parsed_address.is_multicast)):
        fail(f"service {service} network address is unsafe")
    if parsed_gateway.is_unspecified or parsed_gateway.is_multicast:
        fail(f"service {service} network gateway is unsafe")
    if service == "coturn" and (address, gateway, prefix) != ("172.30.247.2", "172.30.247.1", 28):
        fail("coturn fixed network address changed")


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def load_edge_environment(runtime_root: str, evidence_path: Path | None = None) -> dict[str, str]:
    path = evidence_path if evidence_path is not None else Path(runtime_root) / "config/edge.env"
    if path.is_symlink() or not path.is_file():
        fail("runtime edge.env is missing or symlinked")
    if evidence_path is not None:
        try:
            info = path.stat()
        except OSError as exc:
            fail(f"cannot inspect recorded runtime edge.env: {exc}")
        if (
            not path.is_absolute() or path.resolve(strict=True) != path
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_uid != os.getuid() or info.st_gid != os.getgid()
            or info.st_nlink != 1 or not 1 <= info.st_size <= 16384
        ):
            fail("recorded runtime edge.env metadata is invalid")
    try:
        text = path.read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read runtime edge.env: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail("runtime edge.env is not canonical ASCII lines")
    result: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail("runtime edge.env contains a malformed line")
        key, value = line.split("=", 1)
        if key in result:
            fail("runtime edge.env contains a duplicate key")
        result[key] = value
    if set(result) != EDGE_ENV_KEYS:
        fail("runtime edge.env allowlist changed")
    for key in (
        "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY", "DJI4G_EDGE_ENROLLMENT_ENABLED",
        "DJI4G_EDGE_MUTATIONS_ENABLED", "DJI4G_EDGE_PUSH_ENABLED",
        "DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
    ):
        if result[key] != "false":
            fail(f"runtime edge.env fail-closed switch changed: {key}")
    return result


def env_dict(value: object, service: str) -> dict[str, str]:
    if not isinstance(value, list) or any(
        not isinstance(item, str) or "=" not in item for item in value
    ):
        fail(f"service {service} environment evidence is invalid")
    pairs = [item.split("=", 1) for item in value]
    result = {key: item for key, item in pairs}
    if len(result) != len(pairs):
        fail(f"service {service} environment contains duplicate keys")
    return result


def verify_compose_labels(
    service: str, labels: dict, image: str, container_id: str,
    runtime_root: str, compose_file: str,
) -> None:
    expected_compose = {
        "com.docker.compose.container-number": "1",
        "com.docker.compose.depends_on": (
            "edge:service_healthy:false" if service == "cloudflared" else ""
        ),
        "com.docker.compose.image": expected_target_id(image),
        "com.docker.compose.oneoff": "False",
        "com.docker.compose.project": PROJECT,
        "com.docker.compose.project.config_files": compose_file,
        "com.docker.compose.project.working_dir": str(Path(compose_file).parent),
        "com.docker.compose.service": service,
        "com.docker.compose.version": "2.40.3",
    }
    for key, expected in expected_compose.items():
        if labels.get(key) != expected:
            fail(f"service {service} Compose label changed: {key}")
    config_hash = labels.get("com.docker.compose.config-hash")
    if not isinstance(config_hash, str) or re.fullmatch(r"[0-9a-f]{64}", config_hash) is None:
        fail(f"service {service} Compose config-hash label is invalid")
    environment_file = labels.get("com.docker.compose.project.environment_file")
    if environment_file not in (None, runtime_root + "/compose.env"):
        fail(f"service {service} Compose environment-file label changed")
    allowed_compose = set(expected_compose) | {
        "com.docker.compose.config-hash",
        "com.docker.compose.project.environment_file",
    }
    actual_compose = {key for key in labels if key.startswith("com.docker.compose.")}
    if actual_compose - allowed_compose:
        fail(f"service {service} carries an unapproved Compose identity label")
    if labels.get("com.docker.compose.replace") is not None:
        fail(f"service {service} is still marked as a replacement container")
    if container_id[:12] == "":
        fail(f"service {service} container ID is invalid")


def verify_process(service: str, container: dict, config: dict, host: dict, edge_env: dict[str, str]) -> None:
    identifier = container.get("Id")
    if config.get("Hostname") != identifier[:12] or config.get("Domainname") not in (None, ""):
        fail(f"service {service} hostname/domain metadata changed")
    exact_interactive = {
        "AttachStdin": False,
        "AttachStdout": True,
        "AttachStderr": True,
        "Tty": False,
        "OpenStdin": False,
        "StdinOnce": False,
        "NetworkDisabled": False,
    }
    for key, expected in exact_interactive.items():
        if config.get(key) is not expected:
            fail(f"service {service} interactive/network config changed: {key}")
    if config.get("MacAddress") not in (None, "") or config.get("OnBuild") not in (None, []):
        fail(f"service {service} inherited unapproved MAC/on-build config")
    if config.get("StopTimeout") is not None or config.get("Shell") not in (None, []):
        fail(f"service {service} stop/shell config changed")
    if service == "edge":
        if env_dict(config.get("Env"), service) != edge_env:
            fail("edge environment differs from the verified runtime file")
        expected_entrypoint, expected_cmd = ["/djonehub-edge"], None
        expected_path, expected_args, expected_workdir = "/djonehub-edge", [], ""
        expected_exposed = {"8080/tcp": {}}
        expected_stop_signal = "SIGTERM"
        expected_health = {
            "Test": ["CMD", "/djonehub-edge-healthcheck"],
            "Interval": 10_000_000_000, "Timeout": 3_000_000_000,
            "StartPeriod": 5_000_000_000, "Retries": 3,
        }
    elif service == "cloudflared":
        if env_dict(config.get("Env"), service) != {
            "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt",
        }:
            fail("cloudflared inherited environment differs from the locked image config")
        expected_entrypoint, expected_cmd = ["cloudflared", "--no-autoupdate"], CLOUDFLARED_COMMAND
        expected_path = "cloudflared"
        expected_args = ["--no-autoupdate", *CLOUDFLARED_COMMAND]
        expected_workdir, expected_exposed = "/home/nonroot", None
        expected_stop_signal = ""
        expected_health = {
            "Test": ["CMD", "cloudflared", "tunnel", "--metrics", "127.0.0.1:2000", "ready"],
            "Interval": 10_000_000_000, "Timeout": 5_000_000_000,
            "StartPeriod": 10_000_000_000, "Retries": 6,
        }
    else:
        # TURN mutation is disabled in this release, so any coturn container can
        # only be a pre-existing invariant. Forbid environment-based secrets and
        # host escape even before exact future image-command approval exists.
        environment = env_dict(config.get("Env"), service)
        if any(re.search(r"(?i)(secret|token|password|private|credential|key)", key) for key in environment):
            fail("coturn environment contains a sensitive/unapproved key")
        return
    if config.get("Entrypoint") != expected_entrypoint or config.get("Cmd") != expected_cmd:
        fail(f"service {service} Config command differs from the audited model")
    if container.get("Path") != expected_path or container.get("Args") != expected_args:
        fail(f"service {service} effective Path/Args differs from Config")
    if config.get("WorkingDir", "") != expected_workdir or config.get("ExposedPorts") != expected_exposed:
        fail(f"service {service} working directory/exposed ports changed")
    if config.get("StopSignal", "") != expected_stop_signal:
        fail(f"service {service} stop signal changed")
    if config.get("Healthcheck") != expected_health:
        fail(f"service {service} healthcheck command/timing changed")


def verify_common_host_attachments(service: str, host: dict) -> None:
    expected_log = {"Type": "json-file", "Config": {"max-size": "10m", "max-file": "3"}}
    if host.get("LogConfig") != expected_log:
        fail(f"service {service} bounded json-file logging changed")
    for key in ("ExtraHosts", "Devices", "DeviceRequests", "VolumesFrom", "Links", "GroupAdd"):
        if host.get(key) not in (None, []):
            fail(f"service {service} has an unapproved host attachment: {key}")
    if host.get("Runtime") != "runc":
        fail(f"service {service} container runtime changed")


def verify_existing_coturn(
    container: dict, config: dict, host: dict, labels: dict, uid_gid: str,
    compose_file: str, identifier: str,
) -> None:
    """Validate a pre-existing TURN service without treating the sentinel as its image."""
    config_image = config.get("Image")
    target_image = container.get("Image")
    if (
        not isinstance(config_image, str)
        or "@sha256:" not in config_image
        or expected_target_id(config_image) != target_image
    ):
        fail("pre-existing coturn lacks an immutable Docker29 target identity")
    environment_file = labels.get("com.docker.compose.project.environment_file")
    if (
        not isinstance(environment_file, str)
        or not environment_file.startswith("/")
        or "//" in environment_file
        or "/../" in environment_file
        or not environment_file.endswith("/compose.env")
    ):
        fail("pre-existing coturn Compose environment-file identity is invalid")
    invariant_root = str(Path(environment_file).parent)
    verify_compose_labels(
        "coturn", labels, config_image, identifier, invariant_root, compose_file
    )
    exact_interactive = {
        "AttachStdin": False, "AttachStdout": True, "AttachStderr": True,
        "Tty": False, "OpenStdin": False, "StdinOnce": False,
        "NetworkDisabled": False,
    }
    if any(config.get(key) is not expected for key, expected in exact_interactive.items()):
        fail("pre-existing coturn interactive/network config changed")
    if config.get("User") != uid_gid or config.get("Volumes") is not None:
        fail("pre-existing coturn user/volume confinement changed")
    if (
        container.get("Platform") != "linux"
        or container.get("AppArmorProfile") != "docker-default"
        or container.get("ProcessLabel") not in (None, "")
        or container.get("MountLabel") not in (None, "")
    ):
        fail("pre-existing coturn platform/AppArmor/label confinement changed")
    environment = env_dict(config.get("Env"), "coturn")
    if any(
        re.search(r"(?i)(secret|token|password|private|credential|key)", key)
        for key in environment
    ):
        fail("coturn environment contains a sensitive/unapproved key")
    constraints = {
        "ReadonlyRootfs": True, "Privileged": False, "CapDrop": ["ALL"],
        "Init": True, "PidsLimit": 128, "Memory": 268_435_456,
        "NanoCpus": 1_000_000_000,
        "NetworkMode": f"{PROJECT}_public-edge-turn",
        "AutoRemove": False, "PublishAllPorts": False,
    }
    for key, expected in constraints.items():
        if host.get(key) != expected or type(host.get(key)) is not type(expected):
            fail(f"pre-existing coturn runtime confinement changed: {key}")
    security = host.get("SecurityOpt")
    if host.get("CapAdd") not in (None, []) or not isinstance(security, list) or \
        len(security) != 3 or set(security) != {
            "no-new-privileges:true", "apparmor=docker-default", "seccomp=builtin"
        }:
        fail("pre-existing coturn capability/security options changed")
    if host.get("RestartPolicy") != {"Name": "no", "MaximumRetryCount": 0}:
        fail("pre-existing coturn restart policy changed")
    verify_common_host_attachments("coturn", host)
    verify_host_defaults("coturn", host)
    verify_network("coturn", container, host)
    verify_mounts("coturn", container, host, invariant_root)


def verify_host_defaults(service: str, host: dict) -> None:
    expected_swap = 134_217_728 if service == "cloudflared" else 268_435_456
    exact = {
        "CgroupParent": "", "MemorySwap": expected_swap,
        "MemoryReservation": 0, "CpuShares": 0,
        "CpuPeriod": 0, "CpuQuota": 0, "CpusetCpus": "", "CpusetMems": "",
        "CpuRealtimePeriod": 0, "CpuRealtimeRuntime": 0,
        "CpuCount": 0, "CpuPercent": 0,
        "BlkioWeight": 0, "ShmSize": 67_108_864, "Isolation": "",
    }
    for key, expected in exact.items():
        if key not in host or host[key] != expected or type(host[key]) is not type(expected):
            fail(f"service {service} unapproved host/resource setting changed: {key}")
    if host.get("Sysctls") not in (None, {}):
        fail(f"service {service} acquired an unapproved sysctl")
    docker29_exact = {
        "ContainerIDFile": "",
        "VolumeDriver": "",
        "Cgroup": "",
        "OomScoreAdj": 0,
        "IOMaximumIOps": 0,
        "IOMaximumBandwidth": 0,
        "ReadonlyPaths": DOCKER29_READONLY_PATHS,
    }
    for key, expected in docker29_exact.items():
        if key not in host or host[key] != expected or type(host[key]) is not type(expected):
            fail(f"service {service} Docker 29 host default changed: {key}")
    verify_masked_paths(host.get("MaskedPaths"), f"service {service}")
    if "Annotations" in host:
        fail(f"service {service} Docker 29 host schema unexpectedly contains Annotations")
    if host.get("MemorySwappiness") is not None:
        fail(f"service {service} memory swappiness must remain unset")
    if host.get("OomKillDisable") not in (None, False):
        fail(f"service {service} OOM-kill policy changed")
    if host.get("StorageOpt") not in (None, {}):
        fail(f"service {service} storage options changed")
    for key in ("Dns", "DnsOptions", "DnsSearch"):
        if host.get(key) not in (None, []):
            fail(f"service {service} DNS override changed: {key}")
    for key in (
        "BlkioWeightDevice", "BlkioDeviceReadBps", "BlkioDeviceWriteBps",
        "BlkioDeviceReadIOps", "BlkioDeviceWriteIOps", "DeviceCgroupRules",
    ):
        if host.get(key) not in (None, []):
            fail(f"service {service} block/device policy changed: {key}")
    ulimits = host.get("Ulimits")
    if service == "coturn":
        if ulimits != [{"Name": "nofile", "Hard": 65536, "Soft": 65536}]:
            fail("coturn ulimit policy changed")
    elif ulimits not in (None, []):
        fail(f"service {service} acquired an unapproved ulimit")


def load_state(path: Path) -> dict:
    if path.is_symlink() or not path.is_file():
        fail(f"Docker inspect evidence is missing or symlinked: {path}")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        fail(f"invalid Docker inspect JSON: {exc}")
    if not isinstance(value, dict) or set(value) != {
        "containers", "networks", "project_network_names"
    }:
        fail("Docker evidence must use the exact containers/networks schema")
    if not isinstance(value["containers"], list):
        fail("Docker container inspect evidence must be a list")
    if not isinstance(value["networks"], dict) or set(value["networks"]) != NETWORK_KEYS:
        fail("Docker network inspect evidence has an invalid schema")
    if any(item is not None and not isinstance(item, dict) for item in value["networks"].values()):
        fail("Docker network inspect evidence must contain objects or null")
    names = value["project_network_names"]
    if not isinstance(names, list) or any(not isinstance(item, str) for item in names):
        fail("project network name evidence is invalid")
    if names != sorted(set(names)):
        fail("project network names must be unique and sorted")
    expected_names = sorted(
        NETWORK_NAMES[key] for key, network in value["networks"].items() if network is not None
    )
    if names != expected_names:
        fail("project network listing and exact network inspections disagree")
    return value


def valid_hex_identifier(value: object, label: str) -> str:
    if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{64}", value) is None:
        fail(f"{label} is not a full lowercase Docker identifier")
    return value


def validate_project_network(kind: str, network: dict, *, allow_members: bool) -> dict[str, dict]:
    if set(network) - NETWORK_ALLOWED_KEYS:
        fail(f"{kind} network inspect schema changed")
    if network.get("Name") != NETWORK_NAMES[kind]:
        fail(f"{kind} network name changed")
    valid_hex_identifier(network.get("Id"), f"{kind} network ID")
    created = network.get("Created")
    if not isinstance(created, str) or re.fullmatch(
        r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+(?:Z|[+-][0-9]{2}:[0-9]{2})", created
    ) is None:
        fail(f"{kind} network creation timestamp is invalid")
    exact = {
        "Scope": "local", "Driver": "bridge", "EnableIPv4": True,
        "EnableIPv6": False, "Internal": False, "Attachable": False,
        "Ingress": False, "ConfigOnly": False,
    }
    for key, expected in exact.items():
        if key not in network or network[key] != expected or type(network[key]) is not type(expected):
            fail(f"{kind} network policy changed: {key}")
    if network.get("Options") not in ({}, None):
        fail(f"{kind} network options changed")
    if network.get("ConfigFrom") not in ({"Network": ""}, {}, None):
        fail(f"{kind} network ConfigFrom changed")
    for key in ("Peers", "Services"):
        if network.get(key) not in (None, [], {}):
            fail(f"{kind} network unexpectedly carries {key}")
    if network.get("Labels") != NETWORK_LABELS[kind]:
        fail(f"{kind} network Compose labels changed")
    ipam = network.get("IPAM")
    if not isinstance(ipam, dict) or set(ipam) - {"Driver", "Options", "Config"}:
        fail(f"{kind} network IPAM schema changed")
    if ipam.get("Driver") != "default" or ipam.get("Options") not in (None, {}):
        fail(f"{kind} network IPAM driver/options changed")
    configs = ipam.get("Config")
    if not isinstance(configs, list) or len(configs) != 1 or not isinstance(configs[0], dict):
        fail(f"{kind} network must have exactly one IPv4 IPAM config")
    config = configs[0]
    if set(config) - {"Subnet", "IPRange", "Gateway", "AuxiliaryAddresses"}:
        fail(f"{kind} network IPAM config schema changed")
    try:
        subnet = ipaddress.IPv4Network(config.get("Subnet"), strict=True)
        gateway = ipaddress.IPv4Address(config.get("Gateway"))
    except (ipaddress.AddressValueError, ValueError, TypeError):
        fail(f"{kind} network subnet/gateway is invalid")
    if not subnet.is_private or gateway not in subnet or gateway in {subnet.network_address, subnet.broadcast_address}:
        fail(f"{kind} network subnet/gateway is unsafe")
    if config.get("IPRange") not in (None, "") or config.get("AuxiliaryAddresses") not in (None, {}):
        fail(f"{kind} network IP range/auxiliary addresses changed")
    if kind == "turn" and (str(subnet), str(gateway)) != ("172.30.247.0/28", "172.30.247.1"):
        fail("TURN network fixed IPAM changed")
    members = network.get("Containers")
    if not isinstance(members, dict):
        fail(f"{kind} network member evidence is invalid")
    if not allow_members and members:
        fail(f"{kind} network unexpectedly has attached containers")
    for identifier, member in members.items():
        valid_hex_identifier(identifier, f"{kind} network member ID")
        if not isinstance(member, dict) or set(member) - {
            "Name", "EndpointID", "MacAddress", "IPv4Address", "IPv6Address"
        }:
            fail(f"{kind} network member schema changed")
        valid_hex_identifier(member.get("EndpointID"), f"{kind} network endpoint ID")
        if not isinstance(member.get("Name"), str) or not member["Name"]:
            fail(f"{kind} network member name is invalid")
        try:
            member_address = ipaddress.IPv4Interface(member.get("IPv4Address"))
        except (ipaddress.AddressValueError, ValueError, TypeError):
            fail(f"{kind} network member IPv4 evidence is invalid")
        if member_address.network != subnet or member_address.ip in {
            subnet.network_address, subnet.broadcast_address, gateway
        }:
            fail(f"{kind} network member address is outside the approved subnet")
        if member.get("IPv6Address") not in (None, ""):
            fail(f"{kind} network member unexpectedly has IPv6")
        member_mac = member.get("MacAddress")
        if not isinstance(member_mac, str) or re.fullmatch(
            r"[0-9a-f]{2}(?::[0-9a-f]{2}){5}", member_mac
        ) is None:
            fail(f"{kind} network member MAC address is invalid")

    # Engine API v1.52 (Docker 29) reports live IPAM allocation statistics.
    # These counters include daemon-reserved addresses that do not appear in
    # Containers, so they are not network identity. Bind their complete schema,
    # configured subnet, integer bounds and accounting total; endpoint identity
    # remains independently bound by the complete Containers map above.
    status = network.get("Status")
    if not isinstance(status, dict) or set(status) != {"IPAM"}:
        fail(f"{kind} network IPAM status schema changed")
    status_ipam = status["IPAM"]
    if not isinstance(status_ipam, dict) or set(status_ipam) != {"Subnets"}:
        fail(f"{kind} network IPAM status payload changed")
    subnets = status_ipam["Subnets"]
    if not isinstance(subnets, dict) or set(subnets) != {str(subnet)}:
        fail(f"{kind} network IPAM status subnet differs from its configuration")
    counters = subnets[str(subnet)]
    if not isinstance(counters, dict) or set(counters) != {
        "IPsInUse", "DynamicIPsAvailable",
    }:
        fail(f"{kind} network IPAM allocation counter schema changed")
    in_use = counters["IPsInUse"]
    available = counters["DynamicIPsAvailable"]
    if (
        isinstance(in_use, bool) or not isinstance(in_use, int) or in_use < len(members)
        or isinstance(available, bool) or not isinstance(available, int) or available < 0
        or in_use + available != subnet.num_addresses
    ):
        fail(f"{kind} network IPAM allocation counters are inconsistent")
    return members


def verify_network_members(
    kind: str, network: dict | None, by_service: dict[str, dict], services: tuple[str, ...],
    *, allow_empty: bool = False,
) -> None:
    expected = {by_service[name]["Id"]: (name, by_service[name]) for name in services if name in by_service}
    if network is None:
        if expected:
            fail(f"{kind} network is absent while approved services exist")
        return
    members = validate_project_network(kind, network, allow_members=True)
    if not expected and allow_empty:
        if members:
            fail(f"{kind} network contains a foreign endpoint")
        return
    if set(members) != set(expected):
        fail(f"{kind} network complete member set contains a missing/foreign endpoint")
    network_id = network["Id"]
    for identifier, (service, container) in expected.items():
        member = members[identifier]
        expected_name = container.get("Name")
        if expected_name != f"/{PROJECT}-{service}-1" or member.get("Name") != expected_name[1:]:
            fail(f"service {service} container/network name changed")
        endpoint = container["NetworkSettings"]["Networks"][NETWORK_NAMES[kind]]
        if endpoint.get("NetworkID") != network_id or endpoint.get("EndpointID") != member["EndpointID"]:
            fail(f"service {service} endpoint IDs disagree with network inspect")
        if endpoint.get("IPAddress") != member["IPv4Address"].split("/", 1)[0]:
            fail(f"service {service} endpoint address disagrees with network inspect")
        if endpoint.get("MacAddress") != member.get("MacAddress"):
            fail(f"service {service} endpoint MAC disagrees with network inspect")
        names = endpoint.get("DNSNames")
        required_names = [expected_name[1:], service, identifier[:12]]
        if names != required_names:
            fail(f"service {service} DNS aliases changed or include an alias hijack")
        aliases = endpoint.get("Aliases")
        if aliases != required_names[:2]:
            fail(f"service {service} legacy aliases changed")
        if service == "coturn":
            if (
                endpoint.get("IPAddress") != "172.30.247.2"
                or endpoint.get("Gateway") != "172.30.247.1"
                or endpoint.get("IPPrefixLen") != 28
                or endpoint.get("IPAMConfig") not in (
                    None, {"IPv4Address": "172.30.247.2"}
                )
            ):
                fail("coturn invariant endpoint no longer uses the fixed TURN address")


def verify_removal_network_members(
    network: dict | None, by_service: dict[str, dict]
) -> None:
    """Reject foreign control endpoints while allowing target runtime drift.

    stop-edge is deliberately able to remove an exact, uniquely identified
    edge/cloudflared container even when its runtime confinement has drifted.
    The shared project network is still a trust boundary: every attached
    endpoint must belong to one of those exact target container IDs/names.
    A drifted target may already be detached, so membership is a subset rather
    than an equality requirement.
    """
    if network is None:
        return
    members = validate_project_network("control", network, allow_members=True)
    allowed = {
        container["Id"]: (service, container)
        for service, container in by_service.items()
        if service in {"edge", "cloudflared"}
    }
    if set(members) - set(allowed):
        fail("control network contains a foreign endpoint during stop-edge")
    for identifier, member in members.items():
        service, container = allowed[identifier]
        expected_name = f"{PROJECT}-{service}-1"
        if container.get("Name") != f"/{expected_name}" or member.get("Name") != expected_name:
            fail("control network target member identity changed during stop-edge")


def expected_target_id(reference: str) -> str:
    if reference.startswith("sha256:"):
        return reference
    if "@sha256:" not in reference:
        fail("configured image reference lacks an exact target digest")
    return "sha256:" + reference.rsplit("@sha256:", 1)[1]


def verify_mounts(service: str, container: dict, host: dict, runtime_root: str) -> None:
    bind_targets = {
        "edge": {
            "/run/config/gateway-public-key.pem": runtime_root + "/config/gateway-public-key.pem",
            "/run/config/access-allowed-emails": runtime_root + "/config/access-allowed-emails",
        },
        "cloudflared": {
            "/run/secrets/cloudflare-tunnel-token": runtime_root + "/secrets/cloudflare-tunnel.token",
        },
        "coturn": {
            "/etc/coturn/turnserver.conf": runtime_root + "/config/turnserver.conf",
            "/run/secrets/turn-tls-cert.pem": runtime_root + "/secrets/turn-tls-cert.pem",
            "/run/secrets/turn-tls-key.pem": runtime_root + "/secrets/turn-tls-key.pem",
        },
    }[service]
    if host.get("Binds") not in (None, []):
        fail(f"service {service} used legacy/auto-creating bind mounts")
    requested = host.get("Mounts")
    if not isinstance(requested, list) or len(requested) != len(bind_targets):
        fail(f"service {service} requested-mount allowlist changed")
    seen: set[str] = set()
    allowed_requested_keys = {
        "Type", "Source", "Target", "ReadOnly", "Consistency", "BindOptions",
        "VolumeOptions", "TmpfsOptions", "ImageOptions",
    }
    for mount in requested:
        if not isinstance(mount, dict) or set(mount) - allowed_requested_keys:
            fail(f"service {service} requested mount schema changed")
        target = mount.get("Target")
        bind_options = mount.get("BindOptions")
        if bind_options is not None:
            allowed_bind_option_keys = {
                "Propagation", "NonRecursive", "CreateMountpoint",
                "ReadOnlyNonRecursive", "ReadOnlyForceRecursive",
            }
            if not isinstance(bind_options, dict) or set(bind_options) - allowed_bind_option_keys:
                fail(f"service {service} requested bind options schema changed")
            if bind_options.get("Propagation") not in (None, "", "rprivate"):
                fail(f"service {service} requested bind propagation changed")
            for key in allowed_bind_option_keys - {"Propagation"}:
                if key in bind_options and bind_options[key] is not False:
                    fail(f"service {service} requested bind option changed: {key}")
        if (
            mount.get("Type") != "bind"
            or target not in bind_targets
            or mount.get("Source") != bind_targets[target]
            or mount.get("ReadOnly") is not True
            or mount.get("Consistency") not in (None, "")
            or mount.get("VolumeOptions") is not None
            or mount.get("TmpfsOptions") is not None
            or mount.get("ImageOptions") is not None
        ):
            fail(f"service {service} requested bind mount is not exact/read-only")
        seen.add(target)
    if seen != set(bind_targets):
        fail(f"service {service} requested bind mount is duplicated/missing")

    mounts = container.get("Mounts")
    if not isinstance(mounts, list):
        fail(f"service {service} resolved mounts are missing")
    resolved: set[str] = set()
    for mount in mounts:
        if not isinstance(mount, dict) or set(mount) - {
            "Type", "Name", "Source", "Destination", "Driver", "Mode", "RW", "Propagation"
        }:
            fail(f"service {service} resolved mount schema changed")
        kind = mount.get("Type")
        target = mount.get("Destination")
        if kind == "tmpfs" and target == "/tmp":
            if mount.get("RW") is not True or mount.get("Source") not in (None, ""):
                fail(f"service {service} resolved tmpfs is invalid")
            continue
        if (
            kind != "bind"
            or target not in bind_targets
            or mount.get("Source") != bind_targets[target]
            or mount.get("RW") is not False
            or mount.get("Name") not in (None, "")
            or mount.get("Driver") not in (None, "")
        ):
            fail(f"service {service} has an unapproved/writable/anonymous mount")
        resolved.add(target)
    if resolved != set(bind_targets):
        fail(f"service {service} resolved bind mount is duplicated/missing")

    size = "16m" if service == "cloudflared" else "32m"
    bytes_size = "16777216" if service == "cloudflared" else "33554432"
    tmpfs = host.get("Tmpfs")
    if not isinstance(tmpfs, dict) or set(tmpfs) != {"/tmp"} or tmpfs["/tmp"] not in {
        f"rw,noexec,nosuid,nodev,size={size},mode=1777",
        f"rw,noexec,nosuid,nodev,size={bytes_size},mode=1777",
    }:
        fail(f"service {service} tmpfs constraint changed")


def index_containers(
    containers: list, expected_images: dict[str, str], uid_gid: str, runtime_root: str,
    compose_file: str, edge_env: dict[str, str], strict_targets: bool = True,
) -> dict[str, dict]:
    by_service: dict[str, dict] = {}
    for container in containers:
        if not isinstance(container, dict):
            fail("Docker inspect contains a non-object")
        config = container.get("Config")
        host = container.get("HostConfig")
        if not isinstance(config, dict) or not isinstance(host, dict):
            fail("container inspection lacks Config/HostConfig")
        labels = config.get("Labels")
        if not isinstance(labels, dict) or labels.get("com.docker.compose.project") != PROJECT:
            fail("container is not bound to the exact Compose project")
        service = labels.get("com.docker.compose.service")
        if service not in ALLOWED:
            fail(f"unapproved/orphan Compose service: {service!r}")
        if service in by_service:
            fail(f"multiple containers exist for Compose service {service}")
        if labels.get("com.docker.compose.oneoff") != "False":
            fail(f"one-off container is forbidden for service {service}")
        identifier = valid_hex_identifier(container.get("Id"), f"service {service} container ID")
        if container.get("Name") != f"/{PROJECT}-{service}-1":
            fail(f"service {service} container name changed")
        if not strict_targets and service in {"edge", "cloudflared"}:
            # stop-edge is the emergency remediation path: once exact project,
            # service, unique ID and container name are established, drift in
            # the target runtime must not prevent its precise removal.
            by_service[service] = container
            continue
        if service == "coturn":
            # TURN has no executable path in this edge-only release.  If an
            # older, separately managed coturn container already exists under
            # this project, treat it only as an invariant baseline: require an
            # immutable Docker29 target identity, a unique project/service
            # identity and strict restart evidence, then compare its exact
            # runtime fingerprint and project network before/after the edge
            # action.  The disabled image sentinel in this runtime root must
            # never be mistaken for the pre-existing container's image.
            verify_existing_coturn(
                container, config, host, labels, uid_gid, compose_file, identifier
            )
            restart_count = container.get("RestartCount")
            if (
                isinstance(restart_count, bool)
                or not isinstance(restart_count, int)
                or restart_count < 0
            ):
                fail("coturn restart count evidence is invalid")
            by_service[service] = container
            continue
        if config.get("Image") != expected_images[service]:
            fail(f"service {service} does not use its exact configured image reference")
        if container.get("Image") != expected_target_id(expected_images[service]):
            fail(f"service {service} container .Image does not equal its containerd target digest")
        if config.get("User") != uid_gid:
            fail(f"service {service} does not use the generated non-root UID:GID")
        verify_compose_labels(
            service, labels, expected_images[service], identifier,
            runtime_root, compose_file,
        )
        if (
            container.get("Platform") != "linux"
            or container.get("AppArmorProfile") != "docker-default"
            or container.get("ProcessLabel") not in (None, "")
            or container.get("MountLabel") not in (None, "")
        ):
            fail(f"service {service} platform/AppArmor/label confinement changed")
        if config.get("Volumes") is not None:
            fail(f"service {service} inherited an image-declared volume")
        verify_process(service, container, config, host, edge_env)
        verify_common_host_attachments(service, host)
        expected_host = {
            "edge": (128, 268435456, 1_000_000_000, f"{PROJECT}_public-edge-control"),
            "cloudflared": (64, 134217728, 500_000_000, f"{PROJECT}_public-edge-control"),
            "coturn": (128, 268435456, 1_000_000_000, f"{PROJECT}_public-edge-turn"),
        }[service]
        constraints = {
            "ReadonlyRootfs": True,
            "Privileged": False,
            "CapDrop": ["ALL"],
            "Init": True,
            "PidsLimit": expected_host[0],
            "Memory": expected_host[1],
            "NanoCpus": expected_host[2],
            "NetworkMode": expected_host[3],
            "AutoRemove": False,
            "PublishAllPorts": False,
        }
        for key, expected in constraints.items():
            if host.get(key) != expected or type(host.get(key)) is not type(expected):
                fail(f"service {service} runtime confinement changed: {key}")
        security = host.get("SecurityOpt")
        if host.get("CapAdd") not in (None, []) or not isinstance(security, list) or \
            len(security) != 3 or set(security) != {
                "no-new-privileges:true", "apparmor=docker-default", "seccomp=builtin"
            }:
            fail(f"service {service} capability/security options changed")
        if host.get("RestartPolicy") != {"Name": "no", "MaximumRetryCount": 0}:
            fail(f"service {service} restart policy changed")
        verify_host_defaults(service, host)
        verify_network(service, container, host)
        restart_count = container.get("RestartCount")
        if isinstance(restart_count, bool) or not isinstance(restart_count, int) or restart_count < 0:
            fail(f"service {service} restart count evidence is invalid")
        verify_mounts(service, container, host, runtime_root)
        if container.get("Id") != identifier:
            fail(f"service {service} container identity changed during verification")
        by_service[service] = container
    return by_service


def coturn_fingerprint(container: dict) -> dict:
    identifier = container.get("Id")
    state = container.get("State")
    restart_count = container.get("RestartCount")
    if not isinstance(identifier, str) or re.fullmatch(r"[0-9a-f]{64}", identifier) is None:
        fail("coturn container ID evidence is invalid")
    if not isinstance(container.get("Image"), str) or not isinstance(state, dict):
        fail("coturn image/state evidence is invalid")
    if not isinstance(restart_count, int) or isinstance(restart_count, bool) or restart_count < 0:
        fail("coturn restart count evidence is invalid")
    stable_state_keys = (
        "Status", "Running", "Paused", "Restarting", "OOMKilled", "Dead",
        "ExitCode", "Error", "StartedAt", "FinishedAt",
    )
    if any(key not in state for key in stable_state_keys):
        fail("coturn state evidence lacks a stable comparison field")
    config = container.get("Config")
    host = container.get("HostConfig")
    mounts = container.get("Mounts")
    if not isinstance(config, dict) or not isinstance(host, dict) or not isinstance(mounts, list):
        fail("coturn config/host/mount evidence is invalid")
    return {
        "Id": identifier,
        "Image": container["Image"],
        "Name": container.get("Name"),
        "Path": container.get("Path"),
        "Args": container.get("Args"),
        "Config": config,
        "HostConfig": host,
        "Mounts": mounts,
        "Platform": container.get("Platform"),
        "AppArmorProfile": container.get("AppArmorProfile"),
        "ProcessLabel": container.get("ProcessLabel"),
        "MountLabel": container.get("MountLabel"),
        "RestartCount": restart_count,
        "State": {key: state[key] for key in stable_state_keys},
        "NetworkSettings": container["NetworkSettings"],
    }


def network_without_dynamic_status(network: dict | None) -> dict | None:
    """Return network identity without Docker 29's live IPAM counters."""
    if network is None:
        return None
    value = json.loads(json.dumps(network))
    value.pop("Status", None)
    return value


def verify_coturn_unchanged(
    before: dict[str, dict], after: dict[str, dict],
    before_turn: dict | None, after_turn: dict | None,
) -> str:
    old = before.get("coturn")
    new = after.get("coturn")
    if network_without_dynamic_status(before_turn) != network_without_dynamic_status(after_turn):
        fail("TURN project network/IPAM/member evidence changed during the edge action")
    if old is None:
        if new is not None:
            fail("coturn appeared although it was absent before the edge action")
        return "absent-before-and-after"
    if new is None:
        fail("coturn disappeared during the edge action")
    if coturn_fingerprint(old) != coturn_fingerprint(new):
        fail("coturn container identity/image/restart/runtime state changed during the edge action")
    return "exact-fingerprint-unchanged"


def verify_stop_control_transition(
    before_network: dict | None, after_network: dict | None,
    before_services: dict[str, dict], after_services: dict[str, dict],
) -> None:
    if any(name in after_services for name in ("edge", "cloudflared")):
        fail("stop-edge left an edge/cloudflared container behind")
    if after_network is not None:
        fail("stop-edge/rollback left the project control network behind")
    # If the network was present before stop, its ID and full member facts are
    # retained in the baseline evidence. The deploy wrapper removes only that
    # exact ID after both selected container IDs disappear.
    if before_network is not None:
        validate_project_network("control", before_network, allow_members=True)


def control_without_members(network: dict) -> dict:
    value = json.loads(json.dumps(network))
    value["Containers"] = {}
    value.pop("Status", None)
    return value


def verify_control_cleanup_checkpoint(
    before_network: dict | None, current_network: dict | None,
    current_services: dict[str, dict], *, rollback: bool,
) -> str | None:
    if any(name in current_services for name in ("edge", "cloudflared")):
        fail("control-network cleanup checkpoint still contains an edge target container")
    if rollback and before_network is not None:
        validate_project_network("control", before_network, allow_members=False)
        if network_without_dynamic_status(current_network) != network_without_dynamic_status(before_network):
            fail("rollback changed a pre-existing empty control network")
        return None
    if current_network is None:
        return None
    members = validate_project_network("control", current_network, allow_members=False)
    assert not members
    if before_network is not None:
        validate_project_network("control", before_network, allow_members=True)
        if control_without_members(current_network) != control_without_members(before_network):
            fail("control network identity/IPAM changed before exact removal")
    return current_network["Id"]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--phase", choices=(
            "pre", "pre-start-edge", "pre-stop-edge", "post-edge",
            "pre-control-cleanup-stop", "pre-control-cleanup-rollback",
            "post-stop-edge", "post-rollback-edge",
        ),
        required=True,
    )
    parser.add_argument("--edge-image", required=True)
    parser.add_argument("--cloudflared-image", required=True)
    parser.add_argument("--coturn-image", required=True)
    parser.add_argument("--uid-gid", required=True)
    parser.add_argument("--runtime-root", required=True)
    parser.add_argument("--edge-env-evidence", type=Path)
    parser.add_argument("--compose-file", required=True)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("json_file", type=Path)
    args = parser.parse_args()
    expected_images = {"edge": args.edge_image, "cloudflared": args.cloudflared_image, "coturn": args.coturn_image}
    if not args.runtime_root.startswith("/") or "//" in args.runtime_root or "/../" in args.runtime_root or args.runtime_root.endswith(("/.", "/..", "/")):
        fail("runtime root must be a canonical absolute path")
    compose_file = Path(args.compose_file)
    if (
        not compose_file.is_absolute()
        or compose_file.is_symlink()
        or not compose_file.is_file()
        or compose_file.resolve() != compose_file
    ):
        fail("Compose file must be a physical canonical regular file")
    edge_env = load_edge_environment(args.runtime_root, args.edge_env_evidence)
    state = load_state(args.json_file)
    strict_targets = args.phase != "pre-stop-edge"
    by_service = index_containers(
        state["containers"], expected_images, args.uid_gid, args.runtime_root,
        args.compose_file, edge_env, strict_targets=strict_targets,
    )
    control = state["networks"]["control"]
    turn = state["networks"]["turn"]

    # TURN is outside this edge-only release but remains an exact invariant if
    # it existed before the action. Its network must never hide foreign peers.
    verify_network_members("turn", turn, by_service, ("coturn",), allow_empty=True)

    coturn_result = None
    remove_control_network_id = None
    needs_baseline = args.phase in {
        "post-edge", "pre-control-cleanup-stop", "pre-control-cleanup-rollback",
        "post-stop-edge", "post-rollback-edge",
    }
    if needs_baseline and args.baseline is None:
        fail(f"{args.phase} requires pre-action baseline evidence")
    if not needs_baseline and args.baseline is not None:
        fail(f"{args.phase} does not accept baseline evidence")
    if args.baseline is not None:
        baseline_state = load_state(args.baseline)
        baseline = index_containers(
            baseline_state["containers"], expected_images, args.uid_gid, args.runtime_root,
            args.compose_file, edge_env, strict_targets=args.phase not in {
                "pre-control-cleanup-stop", "post-stop-edge",
            },
        )
        baseline_turn = baseline_state["networks"]["turn"]
        verify_network_members("turn", baseline_turn, baseline, ("coturn",), allow_empty=True)
        coturn_result = verify_coturn_unchanged(baseline, by_service, baseline_turn, turn)

    if args.phase == "pre-start-edge":
        if "edge" in by_service or "cloudflared" in by_service:
            fail("start-edge requires edge and cloudflared to be absent before mutation")
        verify_network_members("control", control, by_service, (), allow_empty=True)
    elif args.phase == "pre-stop-edge":
        verify_removal_network_members(control, by_service)
    elif args.phase == "post-edge":
        for service in ("edge", "cloudflared"):
            state = by_service.get(service, {}).get("State")
            if not isinstance(state, dict) or not state.get("Running") or state.get("Status") != "running":
                fail(f"service {service} is not running after start-edge")
            if state.get("Restarting") is not False or by_service[service]["RestartCount"] != 0:
                fail(f"service {service} restarted during startup")
        for service in ("edge", "cloudflared"):
            health = by_service[service]["State"].get("Health")
            if not isinstance(health, dict) or health.get("Status") != "healthy":
                fail(f"service {service} is not healthy after start-edge")
        verify_network_members("control", control, by_service, ("edge", "cloudflared"))
    elif args.phase in {"pre-control-cleanup-stop", "pre-control-cleanup-rollback"}:
        remove_control_network_id = verify_control_cleanup_checkpoint(
            baseline_state["networks"]["control"], control, by_service,
            rollback=args.phase == "pre-control-cleanup-rollback",
        )
    elif args.phase == "post-stop-edge":
        verify_stop_control_transition(
            baseline_state["networks"]["control"], control, baseline, by_service
        )
    elif args.phase == "post-rollback-edge":
        if any(name in by_service for name in ("edge", "cloudflared")):
            fail("rollback left an edge/cloudflared container behind")
        if network_without_dynamic_status(control) != network_without_dynamic_status(
            baseline_state["networks"]["control"]
        ):
            fail("rollback did not restore the exact pre-action control-network state")
    elif args.phase == "pre":
        verify_network_members(
            "control", control, by_service, tuple(name for name in ("edge", "cloudflared") if name in by_service),
            allow_empty=True,
        )

    print(json.dumps({"format": 1, "phase": args.phase, "services": sorted(by_service),
                      "orphan_free": True, "coturn_invariant": coturn_result,
                      "remove_control_network_id": remove_control_network_id},
                     sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
