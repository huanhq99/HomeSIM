#!/usr/bin/env python3
"""Verify a complete edge image audit bundle without contacting Docker."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import re
import sys
from pathlib import Path


HEX64 = re.compile(r"[0-9a-f]{64}")
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}")
REPO_DIGEST = re.compile(r"[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}")
REQUIRED_ARTIFACTS = {
    "build.json",
    "cve-report.json",
    "edge-image-reference.txt",
    "local-image-retention-reference.txt",
    "grype-db-status.json",
    "image-inspect.json",
    "image.tar",
    "image.tar.sha256",
    "rootfs-report.json",
    "runtime-compose.env",
    "runtime-edge.env",
    "runtime-gateway-public-key.pem",
    "runtime-access-allowed-emails",
    "sbom.spdx.json",
    "startup-inspect.json",
    "startup.log",
    "source.lock",
    "toolchain.lock",
}
PUBLICATION_ARTIFACTS = {"publication-inspect.json", "publication-record.json"}
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


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def no_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=no_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"invalid JSON artifact {path.name}: {exc}")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_env(path: Path, expected_keys: set[str]) -> dict[str, str]:
    try:
        text = path.read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read runtime input {path.name}: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail(f"runtime input {path.name} is not canonical text")
    values: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail(f"runtime input {path.name} contains a malformed line")
        key, value = line.split("=", 1)
        if key in values or not value:
            fail(f"runtime input {path.name} contains a duplicate/empty value")
        values[key] = value
    if set(values) != expected_keys:
        fail(f"runtime input {path.name} allowlist changed")
    return values


def verify_docker29_linux_host_defaults(host: dict, context: str) -> None:
    """Bind the target daemon defaults that carry confinement semantics."""
    exact = {
        "ContainerIDFile": "",
        "VolumeDriver": "",
        "Cgroup": "",
        "OomScoreAdj": 0,
        "IOMaximumIOps": 0,
        "IOMaximumBandwidth": 0,
        "ReadonlyPaths": DOCKER29_READONLY_PATHS,
        "CpuRealtimePeriod": 0,
        "CpuRealtimeRuntime": 0,
        "CpuCount": 0,
        "CpuPercent": 0,
    }
    for key, expected in exact.items():
        if key not in host or host[key] != expected or type(host[key]) is not type(expected):
            fail(f"{context} Docker 29 host default changed: {key}")
    verify_masked_paths(host.get("MaskedPaths"), context)
    if "Annotations" in host:
        fail(f"{context} Docker 29 host schema unexpectedly contains Annotations")


def load_build_verifier():
    path = Path(__file__).with_name("verify-build-bundle.py")
    sys.dont_write_bytecode = True
    spec = importlib.util.spec_from_file_location("public_edge_verify_build_bundle", path)
    if spec is None or spec.loader is None:
        fail("cannot load verify-build-bundle.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def inspect_image(path: Path, expected_id: str, source_digest: str) -> dict[str, str]:
    value = load_json(path)
    if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
        fail("image-inspect.json must contain exactly one image")
    image = value[0]
    config = image.get("Config")
    if image.get("Id") != expected_id or image.get("Os") != "linux" or image.get("Architecture") != "amd64":
        fail("image identity or architecture evidence does not match")
    if not isinstance(config, dict) or config.get("Entrypoint") != ["/djonehub-edge"] or config.get("Cmd") not in (None, []):
        fail("runtime entrypoint evidence is invalid")
    if config.get("User") != "65532:65532":
        fail("image default user evidence is invalid")
    labels = config.get("Labels")
    if not isinstance(labels, dict) or labels.get("io.maccellular.source-tree-sha256") != source_digest or labels.get("io.maccellular.runtime") != "scratch-no-shell":
        fail("image source/runtime labels do not match")
    health = config.get("Healthcheck")
    if not isinstance(health, dict) or health.get("Test") != ["CMD", "/djonehub-edge-healthcheck"]:
        fail("image healthcheck evidence is invalid")
    return labels


def inspect_startup(
    path: Path, expected_id: str, uid_gid: str, runtime_root: str,
    expected_labels: dict[str, str], audit_nonce: str, audit_mount_root: str,
    runtime_environment: dict[str, str],
) -> None:
    value = load_json(path)
    if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
        fail("startup-inspect.json must contain one container")
    container = value[0]
    state = container.get("State")
    config = container.get("Config")
    host = container.get("HostConfig")
    mounts = container.get("Mounts")
    if (
        container.get("Platform") != "linux"
        or container.get("AppArmorProfile") != "docker-default"
        or container.get("ProcessLabel") not in (None, "")
        or container.get("MountLabel") not in (None, "")
    ):
        fail("startup platform/AppArmor/label confinement changed")
    if (
        container.get("Image") != expected_id
        or container.get("Name") != f"/dji4g-edge-audit-{audit_nonce}-startup"
        or container.get("Path") != "/djonehub-edge"
        or container.get("Args") not in (None, [])
    ):
        fail("startup evidence did not bind the exact image command")
    if not isinstance(state, dict) or state.get("Running") is not True or state.get("Status") != "running":
        fail("startup evidence did not bind the exact running image")
    for key, expected in (("Restarting", False), ("OOMKilled", False), ("Dead", False), ("ExitCode", 0)):
        if state.get(key) != expected or type(state.get(key)) is not type(expected):
            fail(f"startup state field is invalid: {key}")
    restart_count = container.get("RestartCount")
    if isinstance(restart_count, bool) or not isinstance(restart_count, int) or restart_count != 0:
        fail("startup evidence restart count is invalid")
    if not isinstance(state.get("Health"), dict) or state["Health"].get("Status") != "healthy":
        fail("startup evidence is not healthy")
    if not isinstance(config, dict) or config.get("User") != uid_gid:
        fail("startup evidence did not use the generated arbitrary UID:GID")
    startup_labels = dict(expected_labels)
    startup_labels.update({
        "io.maccellular.audit-nonce": audit_nonce,
        "io.maccellular.audit-role": "startup",
    })
    if config.get("Image") != expected_id or config.get("Labels") != startup_labels:
        fail("startup Config image/labels differ from the audited image")
    if config.get("Hostname") != container.get("Id", "")[:12] or config.get("Domainname") not in (None, ""):
        fail("startup hostname/domain metadata changed")
    for key in (
        "AttachStdin", "AttachStdout", "AttachStderr", "Tty", "OpenStdin",
        "StdinOnce", "NetworkDisabled",
    ):
        if config.get(key) is not False:
            fail(f"startup interactive/network config changed: {key}")
    if (
        config.get("MacAddress") not in (None, "")
        or config.get("OnBuild") not in (None, [])
        or config.get("StopTimeout") is not None
        or config.get("Shell") not in (None, [])
        or config.get("StopSignal") != "SIGTERM"
    ):
        fail("startup inherited unapproved MAC/on-build/stop/shell config")
    if config.get("Entrypoint") != ["/djonehub-edge"] or config.get("Cmd") not in (None, []):
        fail("startup container command metadata is invalid")
    expected_health = {
        "Test": ["CMD", "/djonehub-edge-healthcheck"],
        "Interval": 10_000_000_000, "Timeout": 3_000_000_000,
        "StartPeriod": 5_000_000_000, "Retries": 3,
    }
    if config.get("Healthcheck") != expected_health or config.get("WorkingDir", "") != "" or config.get("ExposedPorts") != {"8080/tcp": {}}:
        fail("startup image health/workdir/exposed-port metadata changed")
    if config.get("Volumes") is not None:
        fail("startup container inherited an image-declared volume")
    env = config.get("Env")
    if not isinstance(env, list) or any(not isinstance(item, str) or "=" not in item for item in env):
        fail("startup environment evidence is invalid")
    pairs = [item.split("=", 1) for item in env]
    environment = {key: value for key, value in pairs}
    expected_env_keys = {
        "DJI4G_EDGE_LISTEN_ADDR", "DJI4G_EDGE_PUBLIC_HOST", "DJI4G_EDGE_GATEWAY_ID",
        "DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE", "DJI4G_ACCESS_TEAM_DOMAIN",
        "DJI4G_ACCESS_AUDIENCE", "DJI4G_ACCESS_ALLOWED_EMAILS_FILE",
        "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY", "DJI4G_EDGE_ENROLLMENT_ENABLED",
        "DJI4G_EDGE_MUTATIONS_ENABLED", "DJI4G_EDGE_PUSH_ENABLED",
        "DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
    }
    if len(environment) != len(pairs) or set(environment) != expected_env_keys:
        fail("startup environment allowlist changed")
    if environment != runtime_environment:
        fail("startup environment differs from the verified runtime snapshot")
    if environment["DJI4G_EDGE_LISTEN_ADDR"] != "0.0.0.0:8080" or environment["DJI4G_EDGE_PUBLIC_HOST"] != "phone.example.com":
        fail("startup public listener/host environment changed")
    if environment["DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE"] != "/run/config/gateway-public-key.pem" or environment["DJI4G_ACCESS_ALLOWED_EMAILS_FILE"] != "/run/config/access-allowed-emails":
        fail("startup file-backed identity environment changed")
    for key in (
        "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY", "DJI4G_EDGE_ENROLLMENT_ENABLED",
        "DJI4G_EDGE_MUTATIONS_ENABLED", "DJI4G_EDGE_PUSH_ENABLED",
        "DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
    ):
        if environment[key] != "false":
            fail(f"startup fail-closed environment changed: {key}")
    if not isinstance(host, dict):
        fail("startup HostConfig evidence is missing")
    exact_host = {
        "ReadonlyRootfs": True,
        "Privileged": False,
        "CapDrop": ["ALL"],
        "Init": True,
        "PidsLimit": 128,
        "Memory": 268435456,
        "MemorySwap": 268435456,
        "MemoryReservation": 0,
        "NanoCpus": 1_000_000_000,
        "CpuShares": 0,
        "CpuPeriod": 0,
        "CpuQuota": 0,
        "CpusetCpus": "",
        "CpusetMems": "",
        "NetworkMode": "none",
        "AutoRemove": False,
        "PublishAllPorts": False,
        "PortBindings": {},
        "PidMode": "",
        "IpcMode": "private",
        "UTSMode": "",
        "UsernsMode": "",
        "CgroupnsMode": "private",
        "Runtime": "runc",
        "CgroupParent": "",
        "BlkioWeight": 0,
        "ShmSize": 67_108_864,
        "Isolation": "",
        "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m", "max-file": "3"}},
    }
    for key, expected in exact_host.items():
        if host.get(key) != expected or type(host.get(key)) is not type(expected):
            fail(f"startup HostConfig constraint is invalid: {key}")
    if host.get("Sysctls") not in (None, {}):
        fail("startup HostConfig contains an unapproved sysctl")
    verify_docker29_linux_host_defaults(host, "startup")
    if host.get("CapAdd") not in (None, []):
        fail("startup added capabilities")
    for key in ("ExtraHosts", "Devices", "DeviceRequests", "VolumesFrom", "Links", "GroupAdd"):
        if host.get(key) not in (None, []):
            fail(f"startup HostConfig contains an unapproved host attachment: {key}")
    security = host.get("SecurityOpt")
    if not isinstance(security, list) or len(security) != 3 or set(security) != {
        "no-new-privileges:true", "apparmor=docker-default", "seccomp=builtin"
    }:
        fail("startup no-new-privileges/AppArmor/seccomp constraints changed")
    if host.get("RestartPolicy") != {"Name": "no", "MaximumRetryCount": 0}:
        fail("startup restart policy is invalid")
    if host.get("MemorySwappiness") is not None or host.get("OomKillDisable") not in (None, False):
        fail("startup memory/OOM defaults changed")
    if host.get("StorageOpt") not in (None, {}) or host.get("Ulimits") not in (None, []):
        fail("startup storage/ulimit defaults changed")
    for key in ("Dns", "DnsOptions", "DnsSearch"):
        if host.get(key) not in (None, []):
            fail(f"startup DNS override changed: {key}")
    for key in (
        "BlkioWeightDevice", "BlkioDeviceReadBps", "BlkioDeviceWriteBps",
        "BlkioDeviceReadIOps", "BlkioDeviceWriteIOps", "DeviceCgroupRules",
    ):
        if host.get(key) not in (None, []):
            fail(f"startup block/device policy changed: {key}")
    tmpfs = host.get("Tmpfs")
    allowed_tmpfs = {
        "rw,noexec,nosuid,nodev,size=32m,mode=1777",
        "rw,noexec,nosuid,nodev,size=33554432,mode=1777",
    }
    if not isinstance(tmpfs, dict) or set(tmpfs) != {"/tmp"} or tmpfs["/tmp"] not in allowed_tmpfs:
        fail("startup tmpfs constraint is invalid")

    expected_binds = {
        "/run/config/gateway-public-key.pem": audit_mount_root + "/runtime-gateway-public-key.pem",
        "/run/config/access-allowed-emails": audit_mount_root + "/runtime-access-allowed-emails",
    }
    if host.get("Binds") not in (None, []):
        fail("startup used legacy/auto-creating bind mounts")
    requested = host.get("Mounts")
    if not isinstance(requested, list) or len(requested) != len(expected_binds):
        fail("startup requested-mount allowlist changed")
    requested_targets: set[str] = set()
    for mount in requested:
        allowed_mount_keys = {
            "Type", "Source", "Target", "ReadOnly", "Consistency", "BindOptions",
            "VolumeOptions", "TmpfsOptions", "ImageOptions",
        }
        if not isinstance(mount, dict) or set(mount) - allowed_mount_keys:
            fail("startup requested mount schema is invalid")
        target = mount.get("Target")
        if mount.get("Type") != "bind" or target not in expected_binds or mount.get("Source") != expected_binds[target] or mount.get("ReadOnly") is not True:
            fail("startup requested bind mount differs from the exact read-only allowlist")
        bind_options = mount.get("BindOptions")
        if bind_options is not None:
            allowed_bind_option_keys = {
                "Propagation", "NonRecursive", "CreateMountpoint",
                "ReadOnlyNonRecursive", "ReadOnlyForceRecursive",
            }
            if not isinstance(bind_options, dict) or set(bind_options) - allowed_bind_option_keys:
                fail("startup requested bind options schema changed")
            if bind_options.get("Propagation") not in (None, "", "rprivate"):
                fail("startup requested bind propagation changed")
            for key in allowed_bind_option_keys - {"Propagation"}:
                if key in bind_options and bind_options[key] is not False:
                    fail(f"startup requested bind option changed: {key}")
        if (
            mount.get("Consistency") not in (None, "")
            or mount.get("VolumeOptions") is not None
            or mount.get("TmpfsOptions") is not None
            or mount.get("ImageOptions") is not None
        ):
            fail("startup requested bind options are not default")
        requested_targets.add(target)
    if requested_targets != set(expected_binds):
        fail("startup requested bind mount is duplicated/missing")

    if not isinstance(mounts, list):
        fail("startup resolved mounts are missing")
    resolved_targets: set[str] = set()
    for mount in mounts:
        if not isinstance(mount, dict) or set(mount) - {"Type", "Name", "Source", "Destination", "Driver", "Mode", "RW", "Propagation"}:
            fail("startup resolved mount schema is invalid")
        kind = mount.get("Type")
        target = mount.get("Destination")
        if kind == "tmpfs" and target == "/tmp":
            if mount.get("RW") is not True or mount.get("Source") not in (None, ""):
                fail("startup resolved tmpfs is invalid")
            continue
        if kind != "bind" or target not in expected_binds or mount.get("Source") != expected_binds[target] or mount.get("RW") is not False:
            fail("startup contains an unapproved/writable/anonymous mount")
        if mount.get("Name") not in (None, "") or mount.get("Driver") not in (None, ""):
            fail("startup bind mount unexpectedly names a volume/driver")
        resolved_targets.add(target)
    if resolved_targets != set(expected_binds):
        fail("startup resolved bind mount is duplicated/missing")

    settings = container.get("NetworkSettings")
    if not isinstance(settings, dict) or settings.get("Ports") != {}:
        fail("startup resolved ports are not empty under --network none")
    networks = settings.get("Networks")
    if not isinstance(networks, dict) or set(networks) != {"none"}:
        fail("startup network membership is not exactly the Docker none network")
    endpoint = networks["none"]
    allowed_endpoint_keys = {
        "IPAMConfig", "Links", "Aliases", "MacAddress", "DriverOpts", "GwPriority",
        "NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen",
        "IPv6Gateway", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "DNSNames",
    }
    if not isinstance(endpoint, dict) or set(endpoint) - allowed_endpoint_keys:
        fail("startup none-network endpoint schema changed")
    for key in ("NetworkID", "EndpointID"):
        if not isinstance(endpoint.get(key), str) or HEX64.fullmatch(endpoint[key]) is None:
            fail(f"startup none-network {key} is invalid")
    for key in ("Gateway", "IPAddress", "IPv6Gateway", "GlobalIPv6Address", "MacAddress"):
        if endpoint.get(key) not in (None, ""):
            fail(f"startup none-network unexpectedly has {key}")
    for key in ("IPPrefixLen", "GlobalIPv6PrefixLen", "GwPriority"):
        if endpoint.get(key) not in (None, 0) or isinstance(endpoint.get(key), bool):
            fail(f"startup none-network {key} changed")
    for key in ("Aliases", "Links", "DNSNames"):
        if endpoint.get(key) not in (None, []):
            fail(f"startup none-network carries aliases/links/DNS names: {key}")
    if endpoint.get("IPAMConfig") is not None or endpoint.get("DriverOpts") not in (None, {}):
        fail("startup none-network IPAM/driver options changed")


def verify_spdx_report(value: object, syft_version: str, audited: dt.datetime) -> None:
    if not isinstance(value, dict):
        fail("SPDX SBOM is not an object")
    required = {
        "spdxVersion", "dataLicense", "SPDXID", "name", "documentNamespace",
        "creationInfo", "packages", "relationships",
    }
    if not required <= set(value):
        fail("SPDX SBOM is missing identity/source fields")
    if value.get("spdxVersion") != "SPDX-2.3" or value.get("dataLicense") != "CC0-1.0" or value.get("SPDXID") != "SPDXRef-DOCUMENT":
        fail("SPDX document identity is invalid")
    if not isinstance(value.get("name"), str) or not value["name"]:
        fail("SPDX document name is missing")
    namespace = value.get("documentNamespace")
    if not isinstance(namespace, str) or not namespace.startswith("https://anchore.com/syft/"):
        fail("SPDX namespace is not Syft-generated")
    creation = value.get("creationInfo")
    if not isinstance(creation, dict) or not isinstance(creation.get("creators"), list):
        fail("SPDX creation metadata is invalid")
    if f"Tool: syft-{syft_version}" not in creation["creators"]:
        fail("SPDX creator does not equal the hash-locked Syft version")
    try:
        created = dt.datetime.fromisoformat(str(creation["created"]).replace("Z", "+00:00"))
    except (KeyError, ValueError):
        fail("SPDX creation time is invalid")
    if created.tzinfo is None or created > audited + dt.timedelta(minutes=5) or audited - created > dt.timedelta(hours=1):
        fail("SPDX creation time is not bound to this audit")
    packages = value.get("packages")
    if not isinstance(packages, list) or not packages:
        fail("SPDX package inventory is empty")
    package_ids: set[str] = set()
    for package in packages:
        if not isinstance(package, dict) or not isinstance(package.get("name"), str) or not package["name"]:
            fail("SPDX package record is invalid")
        identifier = package.get("SPDXID")
        if not isinstance(identifier, str) or not identifier.startswith("SPDXRef-") or identifier in package_ids:
            fail("SPDX package identifier is invalid/duplicate")
        package_ids.add(identifier)
    relationships = value.get("relationships")
    if not isinstance(relationships, list):
        fail("SPDX relationship inventory is invalid")
    describes: list[str] = []
    for relationship in relationships:
        if not isinstance(relationship, dict):
            fail("SPDX relationship record is invalid")
        if (
            relationship.get("spdxElementId") == "SPDXRef-DOCUMENT"
            and relationship.get("relationshipType") == "DESCRIBES"
        ):
            target = relationship.get("relatedSpdxElement")
            if not isinstance(target, str):
                fail("SPDX document DESCRIBES target is invalid")
            describes.append(target)
    if len(describes) != 1 or describes[0] not in package_ids:
        fail("SPDX document lacks one package-bound DESCRIBES relationship")
    document_describes = value.get("documentDescribes")
    if document_describes is not None and document_describes != describes:
        fail("SPDX documentDescribes disagrees with its relationship inventory")


def verify_grype_report(
    value: object, grype_version: str, schema: str, status: dict, audited: dt.datetime
) -> None:
    if not isinstance(value, dict):
        fail("Grype CVE report is not an object")
    required = {"matches", "source", "distro", "descriptor"}
    if not required <= set(value):
        fail("Grype CVE report lacks scanner/source/ignored-match evidence")
    matches = value.get("matches")
    ignored = value.get("ignoredMatches", [])
    if not isinstance(matches, list) or ignored != []:
        fail("Grype match/ignored-match evidence is invalid")
    descriptor = value.get("descriptor")
    if not isinstance(descriptor, dict) or descriptor.get("name") != "grype" or descriptor.get("version") != grype_version:
        fail("Grype descriptor does not equal the hash-locked scanner")
    if not isinstance(descriptor.get("configuration"), dict):
        fail("Grype effective configuration is missing")
    database = descriptor.get("db")
    if not isinstance(database, dict) or set(database) != {"status", "providers"}:
        fail("Grype report database identity is missing/malformed")
    if database.get("status") != status:
        fail("Grype report database status differs from the controlled DB-status artifact")
    if not isinstance(database.get("providers"), dict):
        fail("Grype report database provider provenance is invalid")
    source = value.get("source")
    if not isinstance(source, dict) or source.get("type") != "sbom-file" or source.get("target") != "/work/sbom.spdx.json":
        fail("Grype report source is not the controlled SPDX artifact")
    distro = value.get("distro")
    if not isinstance(distro, dict) or not {"name", "version", "idLike"} <= set(distro):
        fail("Grype report distro context is missing/malformed")
    allowed_severities = {"Unknown", "Negligible", "Low", "Medium", "High", "Critical"}
    for match in matches:
        if not isinstance(match, dict) or not isinstance(match.get("artifact"), dict):
            fail("Grype match record is invalid")
        vulnerability = match.get("vulnerability")
        if not isinstance(vulnerability, dict) or not isinstance(vulnerability.get("id"), str):
            fail("Grype vulnerability identity is invalid")
        severity = vulnerability.get("severity")
        if severity not in allowed_severities:
            fail("Grype vulnerability severity is unknown")
        if severity in {"High", "Critical"}:
            fail("CVE report contains a high/critical finding")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--image", required=True)
    parser.add_argument("--repo-root", required=True, type=Path)
    parser.add_argument("--trusted-source-lock", required=True, type=Path)
    parser.add_argument("--trusted-toolchain-lock", required=True, type=Path)
    parser.add_argument("--max-age-hours", type=int, default=48)
    parser.add_argument(
        "--as-of",
        help="RFC3339 UTC evidence-use time for archival replay; defaults to now",
    )
    args = parser.parse_args()
    if IMAGE_ID.fullmatch(args.image) is None and REPO_DIGEST.fullmatch(args.image) is None:
        fail("expected image must be an exact local ID or repository digest")
    root = args.evidence
    if root.is_symlink() or not root.is_dir():
        fail("evidence must be a non-symlink directory")
    manifest = load_json(root / "evidence.json")
    expected_keys = {
        "format", "audited_at", "image_id", "accepted_references", "source_tree_sha256",
        "runtime_uid_gid", "runtime_root", "audit_nonce", "audit_input_mount_root",
        "runtime_input_sha256", "policy", "artifacts",
    }
    if not isinstance(manifest, dict) or set(manifest) != expected_keys or manifest.get("format") != 1:
        fail("evidence manifest schema changed")
    image_id = manifest.get("image_id")
    source_digest = manifest.get("source_tree_sha256")
    uid_gid = manifest.get("runtime_uid_gid")
    runtime_root = manifest.get("runtime_root")
    audit_nonce = manifest.get("audit_nonce")
    audit_mount_root = manifest.get("audit_input_mount_root")
    references = manifest.get("accepted_references")
    if not isinstance(image_id, str) or IMAGE_ID.fullmatch(image_id) is None:
        fail("evidence image ID is invalid")
    if not isinstance(source_digest, str) or HEX64.fullmatch(source_digest) is None:
        fail("evidence source digest is invalid")
    if not isinstance(uid_gid, str) or re.fullmatch(r"[1-9][0-9]*:[1-9][0-9]*", uid_gid) is None:
        fail("evidence runtime UID:GID is invalid")
    if not isinstance(runtime_root, str) or not runtime_root.startswith("/") or "//" in runtime_root or "/../" in runtime_root or runtime_root.endswith(("/.", "/..", "/")):
        fail("evidence runtime root is not a canonical absolute path")
    if not isinstance(audit_nonce, str) or re.fullmatch(r"[0-9a-f]{32}", audit_nonce) is None:
        fail("evidence audit nonce is invalid")
    if (
        not isinstance(audit_mount_root, str)
        or not audit_mount_root.startswith("/")
        or "//" in audit_mount_root
        or "/../" in audit_mount_root
        or audit_mount_root.endswith(("/.", "/..", "/"))
        or audit_mount_root == runtime_root
        or re.fullmatch(r"\.dji4g-edge-audit\.[A-Za-z0-9]+", Path(audit_mount_root).name) is None
    ):
        fail("evidence audit input mount root is invalid")
    if not isinstance(references, list) or image_id not in references or args.image not in references or len(set(references)) != len(references):
        fail("requested image is not bound by the evidence")
    policy = manifest.get("policy")
    build_verifier = load_build_verifier()
    toolchain = build_verifier.read_canonical_lock(
        args.trusted_toolchain_lock, build_verifier.TOOLCHAIN_KEYS
    )
    try:
        db_max_age_hours = int(toolchain["GRYPE_DB_MAX_AGE_HOURS"])
    except (KeyError, ValueError):
        fail("trusted Grype DB freshness lock is invalid")
    if db_max_age_hours != 48 or toolchain.get("GRYPE_DB_SCHEMA_VERSION") != "v6.1.9":
        fail("trusted Grype DB policy changed")
    expected_policy = {
        "architecture": "linux/amd64",
        "cve_threshold": "high",
        "health": "passed",
        "read_only_arbitrary_uid": "passed",
        "rootfs": "scratch-no-shell",
        "sbom": "spdx-json",
        "startup": "passed",
        "grype_version": toolchain["GRYPE_VERSION"],
        "grype_db_schema": toolchain["GRYPE_DB_SCHEMA_VERSION"],
        "grype_db_max_age_hours": db_max_age_hours,
    }
    if policy != expected_policy:
        fail("evidence policy gates are incomplete")
    try:
        audited = dt.datetime.fromisoformat(str(manifest["audited_at"]).replace("Z", "+00:00"))
    except ValueError:
        fail("evidence audit time is invalid")
    if args.as_of is None:
        now = dt.datetime.now(dt.timezone.utc)
    else:
        if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", args.as_of) is None:
            fail("archival evidence-use time is not canonical RFC3339 UTC")
        try:
            now = dt.datetime.fromisoformat(args.as_of.replace("Z", "+00:00"))
        except ValueError:
            fail("archival evidence-use time is invalid")
    if audited.tzinfo is None or audited > now + dt.timedelta(minutes=5) or now - audited > dt.timedelta(hours=args.max_age_hours):
        fail("image evidence is stale or from the future")
    artifacts = manifest.get("artifacts")
    artifact_names = set(artifacts) if isinstance(artifacts, dict) else set()
    if artifact_names not in (REQUIRED_ARTIFACTS, REQUIRED_ARTIFACTS | PUBLICATION_ARTIFACTS):
        fail("evidence artifact allowlist changed")
    expected_directory = artifact_names | {"evidence.json"}
    try:
        actual_directory = {path.name for path in root.iterdir()}
    except OSError as exc:
        fail(f"cannot enumerate evidence directory: {exc}")
    if actual_directory != expected_directory or any(path.is_symlink() or not path.is_file() for path in root.iterdir()):
        fail("evidence directory contains an unapproved entry")
    for name, expected in artifacts.items():
        if Path(name).name != name or not isinstance(expected, str) or HEX64.fullmatch(expected) is None:
            fail("evidence artifact entry is invalid")
        path = root / name
        if path.is_symlink() or not path.is_file() or sha256(path) != expected:
            fail(f"evidence artifact hash mismatch: {name}")

    runtime_input_names = {
        "compose.env": "runtime-compose.env",
        "config/edge.env": "runtime-edge.env",
        "config/gateway-public-key.pem": "runtime-gateway-public-key.pem",
        "config/access-allowed-emails": "runtime-access-allowed-emails",
    }
    runtime_input_sha256 = manifest.get("runtime_input_sha256")
    if (
        not isinstance(runtime_input_sha256, dict)
        or set(runtime_input_sha256) != set(runtime_input_names)
        or any(runtime_input_sha256[key] != artifacts[runtime_input_names[key]] for key in runtime_input_names)
    ):
        fail("runtime input digest binding is incomplete")
    compose_environment = canonical_env(root / "runtime-compose.env", {
        "DJI4G_PUBLIC_EDGE_ROOT", "DJI4G_EDGE_ENV_FILE", "DJI4G_ENABLED_PROFILES",
        "DJI4G_EDGE_UID", "DJI4G_EDGE_GID", "DJI4G_EDGE_IMAGE",
        "DJI4G_CLOUDFLARED_IMAGE", "DJI4G_COTURN_IMAGE",
    })
    if (
        compose_environment["DJI4G_PUBLIC_EDGE_ROOT"] != runtime_root
        or compose_environment["DJI4G_EDGE_ENV_FILE"] != runtime_root + "/config/edge.env"
        or compose_environment["DJI4G_ENABLED_PROFILES"] != "edge"
        or f'{compose_environment["DJI4G_EDGE_UID"]}:{compose_environment["DJI4G_EDGE_GID"]}' != uid_gid
        or compose_environment["DJI4G_EDGE_IMAGE"] != image_id
        or compose_environment["DJI4G_CLOUDFLARED_IMAGE"] != toolchain["CLOUDFLARED_IMAGE"]
        or compose_environment["DJI4G_COTURN_IMAGE"] != "invalid.local/dji4g-coturn-disabled@sha256:" + "0" * 64
    ):
        fail("snapshotted compose.env differs from the audited edge-only identity")
    runtime_environment = canonical_env(root / "runtime-edge.env", {
        "DJI4G_EDGE_LISTEN_ADDR", "DJI4G_EDGE_PUBLIC_HOST", "DJI4G_EDGE_GATEWAY_ID",
        "DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE", "DJI4G_ACCESS_TEAM_DOMAIN",
        "DJI4G_ACCESS_AUDIENCE", "DJI4G_ACCESS_ALLOWED_EMAILS_FILE",
        "DJI4G_EDGE_TRUST_FORWARDED_IDENTITY", "DJI4G_EDGE_ENROLLMENT_ENABLED",
        "DJI4G_EDGE_MUTATIONS_ENABLED", "DJI4G_EDGE_PUSH_ENABLED",
        "DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
    })
    try:
        public_key = (root / "runtime-gateway-public-key.pem").read_bytes()
        allowed_email_text = (root / "runtime-access-allowed-emails").read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read snapshotted public identity input: {exc}")
    normalized_public_key = public_key[:-1] if public_key.endswith(b"\n") else public_key
    if (
        not public_key or len(public_key) > 8192 or b"\r" in public_key or b"\x00" in public_key
        or not normalized_public_key.startswith(b"-----BEGIN PUBLIC KEY-----\n")
        or not normalized_public_key.endswith(b"\n-----END PUBLIC KEY-----")
    ):
        fail("snapshotted gateway public key encoding is invalid")
    normalized_emails = allowed_email_text[:-1] if allowed_email_text.endswith("\n") else allowed_email_text
    allowed_emails = normalized_emails.split("\n")
    email_local = r"[A-Za-z0-9]+(?:[._%+\-][A-Za-z0-9]+)*"
    email_label = r"[A-Za-z0-9](?:[A-Za-z0-9\-]{0,61}[A-Za-z0-9])?"
    email_pattern = re.compile(email_local + r"@" + email_label + r"(?:\." + email_label + r")+")
    if (
        not normalized_emails
        or normalized_emails.endswith("\n")
        or "\n\n" in normalized_emails
        or not allowed_emails
        or len(allowed_emails) > 8
        or len(set(allowed_emails)) != len(allowed_emails)
        or any(len(item) > 254 or email_pattern.fullmatch(item) is None for item in allowed_emails)
    ):
        fail("snapshotted Access allowlist encoding is invalid")

    bound_image_id = build_verifier.verify_build_bundle(
        root,
        args.repo_root.resolve(),
        args.trusted_source_lock,
        args.trusted_toolchain_lock,
        False,
    )
    build_manifest = load_json(root / "build.json")
    if bound_image_id != image_id or build_manifest.get("source_tree_sha256") != source_digest:
        fail("evidence manifest differs from the verified build/archive identity")

    image_labels = inspect_image(root / "image-inspect.json", image_id, source_digest)
    inspect_startup(
        root / "startup-inspect.json", image_id, uid_gid, runtime_root, image_labels,
        audit_nonce, audit_mount_root, runtime_environment,
    )
    published = [value for value in references if value != image_id]
    if published:
        if len(published) != 1 or not PUBLICATION_ARTIFACTS <= artifact_names or REPO_DIGEST.fullmatch(published[0]) is None:
            fail("published reference evidence is incomplete")
        publication = load_json(root / "publication-inspect.json")
        if not isinstance(publication, list) or len(publication) != 1 or publication[0].get("Id") != image_id:
            fail("published reference does not resolve to the audited image ID")
        if published[0] not in publication[0].get("RepoDigests", []):
            fail("published repository digest is absent from Docker evidence")
        record = load_json(root / "publication-record.json")
        expected_record_keys = {
            "format", "immutable_reference", "remote_nonce_tag", "repository",
            "requested_tag", "remote_nonce_retention", "response_loss_policy",
        }
        if not isinstance(record, dict) or set(record) != expected_record_keys or record.get("format") != 1:
            fail("publication lifecycle record schema is invalid")
        repository = record.get("repository")
        requested_tag = record.get("requested_tag")
        remote_nonce_tag = record.get("remote_nonce_tag")
        if (
            not isinstance(repository, str)
            or not isinstance(requested_tag, str)
            or not isinstance(remote_nonce_tag, str)
            or published[0].split("@", 1)[0] != repository
            or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", requested_tag) is None
            or re.fullmatch(re.escape(repository) + r":" + re.escape(requested_tag) + r"-[0-9a-f]{32}", remote_nonce_tag) is None
            or record.get("immutable_reference") != published[0]
            or record.get("remote_nonce_retention") != "not-automatically-deleted"
            or record.get("response_loss_policy") != "remote-nonce-tag-may-exist-reconcile-before-retry"
        ):
            fail("publication lifecycle/response-loss record is invalid")
    elif artifact_names & PUBLICATION_ARTIFACTS:
        fail("unexpected publication evidence")
    status = load_json(root / "grype-db-status.json")
    expected_status_keys = {"schemaVersion", "from", "built", "path", "valid"}
    if not isinstance(status, dict) or set(status) != expected_status_keys:
        fail("Grype database status evidence schema is invalid")
    if status.get("schemaVersion") != toolchain["GRYPE_DB_SCHEMA_VERSION"] or status.get("valid") is not True:
        fail("Grype database schema/status is invalid")
    if status.get("path") != "/work/grype-db/6/vulnerability.db":
        fail("Grype database path is outside the controlled cache")
    source = status.get("from")
    if not isinstance(source, str) or re.fullmatch(
        r"https://grype\.anchore\.io/databases/v6/vulnerability-db_v6\.[0-9.]+_[0-9TZ:+-]+_[0-9]+\.tar\.zst\?checksum=sha256%3A[0-9a-f]{64}",
        source,
    ) is None:
        fail("Grype database source is not the expected authenticated HTTPS origin")
    try:
        built = dt.datetime.fromisoformat(str(status["built"]).replace("Z", "+00:00"))
    except ValueError:
        fail("Grype database built time is invalid")
    if built.tzinfo is None or built > audited + dt.timedelta(minutes=5):
        fail("Grype database built time is missing a zone or is in the future")
    if audited - built > dt.timedelta(hours=db_max_age_hours):
        fail("Grype database is stale")
    sbom = load_json(root / "sbom.spdx.json")
    verify_spdx_report(sbom, toolchain["SYFT_VERSION"], audited)
    cve = load_json(root / "cve-report.json")
    verify_grype_report(
        cve, toolchain["GRYPE_VERSION"], toolchain["GRYPE_DB_SCHEMA_VERSION"], status, audited
    )
    rootfs = load_json(root / "rootfs-report.json")
    if rootfs != {
        "architecture": "amd64",
        "ca_bundle": True,
        "edge_static_elf": True,
        "healthcheck_static_elf": True,
        "no_credentials": True,
        "no_shell": True,
        "no_source": True,
    }:
        fail("rootfs evidence is incomplete")
    log = (root / "startup.log").read_bytes()
    if len(log) > 65536 or b"Public edge read-only service is ready" not in log:
        fail("startup log evidence is missing or unbounded")
    if any(marker in log for marker in (b"PRIVATE KEY", b"CF-Access-Client-Secret", b"TUNNEL_TOKEN", b"sms_body")):
        fail("startup log evidence contains a forbidden value")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
