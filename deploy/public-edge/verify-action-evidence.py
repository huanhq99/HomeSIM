#!/usr/bin/env python3
"""Create or verify a self-hashed public-edge action evidence manifest."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path


HEX64 = re.compile(r"[0-9a-f]{64}")
IMAGE = re.compile(r"(?:sha256:[0-9a-f]{64}|[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64})")
TARGET_ID = re.compile(r"sha256:[0-9a-f]{64}")
CONTAINER_ID = re.compile(r"[0-9a-f]{64}")
SNAPSHOT_NONCE = re.compile(r"[A-Za-z0-9]{6}")
RUNTIME_ENV_KEYS = (
    "DJI4G_PUBLIC_EDGE_ROOT",
    "DJI4G_EDGE_ENV_FILE",
    "DJI4G_ENABLED_PROFILES",
    "DJI4G_EDGE_UID",
    "DJI4G_EDGE_GID",
    "DJI4G_EDGE_IMAGE",
    "DJI4G_CLOUDFLARED_IMAGE",
    "DJI4G_COTURN_IMAGE",
)
RUNTIME_INPUT_FILES = {
    "compose.env": "compose.env",
    "config/edge.env": "config/edge.env",
    "config/gateway-public-key.pem": "config/gateway-public-key.pem",
    "config/access-allowed-emails": "config/access-allowed-emails",
}
RUNTIME_INPUT_EVIDENCE_FILES = {
    "compose.env": "runtime-input-compose.env",
    "config/edge.env": "runtime-input-edge.env",
    "config/gateway-public-key.pem": "runtime-input-gateway-public-key.pem",
    "config/access-allowed-emails": "runtime-input-access-allowed-emails",
}
RUNTIME_INPUT_ANCHOR = "runtime-inputs.pre.json"
RUNTIME_INPUT_ARTIFACTS = {RUNTIME_INPUT_ANCHOR, *RUNTIME_INPUT_EVIDENCE_FILES.values()}
COMPLETED_ARTIFACTS = {
    "start-edge": {
        "pre-inspect.json", "pre-summary.json", "post-inspect.json",
        "post-summary.json", "startup.log", "result.txt", *RUNTIME_INPUT_ARTIFACTS,
    },
    "stop-edge": {
        "pre-inspect.json", "pre-summary.json", "pre-network-remove.json",
        "pre-network-remove-summary.json", "post-inspect.json",
        "post-summary.json", "result.txt", *RUNTIME_INPUT_ARTIFACTS,
    },
}
FAILED_ARTIFACTS = {
    "result.txt", "failure-detail.txt", "failure-startup.log",
    "pre-inspect.json", "pre-summary.json", "failure-inspect.json",
    "failure-summary.json", "pre-network-remove.json",
    "pre-network-remove-summary.json", "rollback-pre-network-remove.json",
    "rollback-pre-network-remove-summary.json", "rollback-inspect.json",
    "rollback-summary.json", *RUNTIME_INPUT_ARTIFACTS,
}


def no_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate key: {key}")
        result[key] = value
    return result


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def canonical_regular(path: Path, label: str) -> Path:
    if path.is_symlink() or not path.is_file():
        fail(f"{label} is missing, symlinked, or not regular")
    try:
        if path.resolve(strict=True) != path:
            fail(f"{label} path is not physical/canonical")
    except OSError as exc:
        fail(f"cannot resolve {label}: {exc}")
    return path


def valid_snapshot_path(configured_root: Path, runtime_root: Path) -> bool:
    prefix = f".{configured_root.name}.edge-snapshot."
    return (
        runtime_root.parent == configured_root.parent
        and runtime_root.name.startswith(prefix)
        and SNAPSHOT_NONCE.fullmatch(runtime_root.name[len(prefix):]) is not None
    )


def parse_runtime_environment(raw: bytes, runtime_root: str, label: str) -> dict[str, str]:
    try:
        text = raw.decode("ascii")
    except UnicodeError as exc:
        fail(f"cannot read {label}: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail("runtime compose.env is not canonical ASCII")
    pairs: list[tuple[str, str]] = []
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail("runtime compose.env contains a malformed line")
        pairs.append(tuple(line.split("=", 1)))
    if tuple(key for key, _ in pairs) != RUNTIME_ENV_KEYS:
        fail("runtime compose.env schema/order changed")
    values = dict(pairs)
    if values["DJI4G_PUBLIC_EDGE_ROOT"] != runtime_root:
        fail("runtime compose.env root differs from action evidence")
    if values["DJI4G_EDGE_ENV_FILE"] != f"{runtime_root}/config/edge.env":
        fail("runtime compose.env edge file differs from action evidence")
    if values["DJI4G_ENABLED_PROFILES"] != "edge":
        fail("action evidence only supports the edge-only runtime profile")
    return values


def load_runtime_environment(runtime_root: str) -> dict[str, str]:
    root = Path(runtime_root)
    if (
        not root.is_absolute() or root.is_symlink() or not root.is_dir()
        or root.resolve() != root
    ):
        fail("action runtime root is not a physical canonical directory")
    path = canonical_regular(root / "compose.env", "runtime compose.env")
    try:
        raw = path.read_bytes()
    except OSError as exc:
        fail(f"cannot read runtime compose.env: {exc}")
    return parse_runtime_environment(raw, runtime_root, "runtime compose.env")


def runtime_input_values(runtime_root: str) -> dict[str, bytes]:
    root = Path(runtime_root)
    try:
        root_info = root.stat()
    except OSError as exc:
        fail(f"cannot inspect action runtime root: {exc}")
    if (
        not root.is_absolute() or root.is_symlink() or not root.is_dir()
        or root.resolve(strict=True) != root
        or root_info.st_uid != os.getuid() or root_info.st_gid != os.getgid()
    ):
        fail("action runtime root is not a physical current-user-owned directory")
    root_mode = stat.S_IMODE(root_info.st_mode)
    if root_mode == 0o500:
        expected_mode = 0o400
    elif root_mode == 0o700:
        expected_mode = 0o600
    else:
        fail("action runtime root mode is neither configured-runtime nor immutable-snapshot mode")
    values: dict[str, bytes] = {}
    for logical, relative in RUNTIME_INPUT_FILES.items():
        path = canonical_regular(root / relative, f"runtime input {logical}")
        descriptor = -1
        try:
            descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            info = os.fstat(descriptor)
        except OSError as exc:
            fail(f"cannot open runtime input {logical}: {exc}")
        try:
            if (
                not stat.S_ISREG(info.st_mode)
                or stat.S_IMODE(info.st_mode) != expected_mode
                or info.st_uid != os.getuid()
                or info.st_gid != os.getgid()
                or info.st_nlink != 1
                or info.st_size < 1
                or info.st_size > 65536
            ):
                fail(f"runtime input {logical} metadata is invalid")
            value = bytearray()
            total = 0
            while True:
                chunk = os.read(descriptor, 1024 * 1024)
                if not chunk:
                    break
                total += len(chunk)
                if total > 65536:
                    fail(f"runtime input {logical} grew beyond its size limit while read")
                value.extend(chunk)
            after = os.fstat(descriptor)
            stable_fields = (
                "st_dev", "st_ino", "st_mode", "st_uid", "st_gid", "st_nlink",
                "st_size", "st_mtime_ns", "st_ctime_ns",
            )
            if total != info.st_size or any(
                getattr(info, field) != getattr(after, field) for field in stable_fields
            ):
                fail(f"runtime input {logical} changed while read")
            values[logical] = bytes(value)
        finally:
            os.close(descriptor)
    return values


def runtime_input_digests(runtime_root: str) -> dict[str, str]:
    return {
        logical: hashlib.sha256(value).hexdigest()
        for logical, value in runtime_input_values(runtime_root).items()
    }


def evidence_runtime_input_values(root: Path) -> dict[str, bytes]:
    values: dict[str, bytes] = {}
    for logical, evidence_name in RUNTIME_INPUT_EVIDENCE_FILES.items():
        values[logical] = stable_source_bytes(
            root / evidence_name, f"recorded runtime input {logical}", 65536
        )
    return values


def evidence_runtime_input_digests(root: Path) -> dict[str, str]:
    return {
        logical: hashlib.sha256(value).hexdigest()
        for logical, value in evidence_runtime_input_values(root).items()
    }


def stable_source_bytes(path: Path, label: str, maximum: int) -> bytes:
    path = canonical_regular(path, label)
    descriptor = -1
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        before = os.fstat(descriptor)
    except OSError as exc:
        fail(f"cannot open {label}: {exc}")
    try:
        if (
            not stat.S_ISREG(before.st_mode)
            or stat.S_IMODE(before.st_mode) != 0o600
            or before.st_uid != os.getuid()
            or before.st_gid != os.getgid()
            or before.st_nlink != 1
            or before.st_size < 1
            or before.st_size > maximum
        ):
            fail(f"{label} metadata is invalid")
        value = bytearray()
        while True:
            chunk = os.read(descriptor, min(65536, maximum + 1))
            if not chunk:
                break
            value.extend(chunk)
            if len(value) > maximum:
                fail(f"{label} grew beyond its size limit while read")
        after = os.fstat(descriptor)
        stable_fields = (
            "st_dev", "st_ino", "st_mode", "st_uid", "st_gid", "st_nlink",
            "st_size", "st_mtime_ns", "st_ctime_ns",
        )
        if len(value) != before.st_size or any(
            getattr(before, field) != getattr(after, field) for field in stable_fields
        ):
            fail(f"{label} changed while read")
        return bytes(value)
    finally:
        os.close(descriptor)


def write_snapshot_file(path: Path, value: bytes) -> None:
    descriptor = -1
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags, 0o600)
        view = memoryview(value)
        while view:
            written = os.write(descriptor, view)
            if written <= 0:
                fail(f"cannot write runtime snapshot file: {path.name}")
            view = view[written:]
        os.fsync(descriptor)
        info = os.fstat(descriptor)
        if (
            not stat.S_ISREG(info.st_mode)
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_uid != os.getuid()
            or info.st_gid != os.getgid()
            or info.st_nlink != 1
            or info.st_size != len(value)
        ):
            fail(f"new runtime snapshot file metadata is invalid: {path.name}")
    except OSError as exc:
        fail(f"cannot create runtime snapshot file {path.name}: {exc}")
    finally:
        if descriptor >= 0:
            os.close(descriptor)


def snapshot_runtime(source_root: str, declared_root: str, output: Path) -> None:
    source = Path(source_root)
    declared = Path(declared_root)
    if (
        not source.is_absolute() or source.is_symlink() or not source.is_dir()
        or source.resolve(strict=True) != source
    ):
        fail("runtime snapshot source must be a physical canonical directory")
    if (
        not declared.is_absolute() or not valid_snapshot_path(source, declared)
        or declared.parent.resolve(strict=True) != declared.parent
        or declared == source
    ):
        fail("runtime snapshot declared path must be the fixed absent source sibling")
    try:
        output_info = output.stat()
    except OSError as exc:
        fail(f"runtime snapshot stage is inaccessible: {exc}")
    if (
        not output.is_absolute() or output.is_symlink() or output.resolve(strict=True) != output
        or output.parent != source.parent
        or not stat.S_ISDIR(output_info.st_mode)
        or stat.S_IMODE(output_info.st_mode) != 0o700
        or output_info.st_uid != os.getuid()
        or output_info.st_gid != os.getgid()
        or any(output.iterdir())
    ):
        fail("runtime snapshot stage must be an empty physical private sibling")
    expected_tree = {
        "compose.env", "config", "config/edge.env",
        "config/gateway-public-key.pem", "config/access-allowed-emails",
        "secrets", "secrets/cloudflare-tunnel.token",
    }
    actual_tree = {str(path.relative_to(source)) for path in source.rglob("*")}
    if actual_tree != expected_tree or any(path.is_symlink() for path in source.rglob("*")):
        fail("runtime snapshot source is not the exact edge-only file tree")
    limits = {
        "compose.env": 4096,
        "config/edge.env": 16384,
        "config/gateway-public-key.pem": 8192,
        "config/access-allowed-emails": 8192,
        "secrets/cloudflare-tunnel.token": 4096,
    }
    original = {
        relative: stable_source_bytes(source / relative, f"runtime snapshot source {relative}", limit)
        for relative, limit in limits.items()
    }
    source_runtime = load_runtime_environment(source_root)
    runtime = dict(source_runtime)
    runtime["DJI4G_PUBLIC_EDGE_ROOT"] = declared_root
    runtime["DJI4G_EDGE_ENV_FILE"] = f"{declared_root}/config/edge.env"
    original_compose = original["compose.env"]
    try:
        source_compose = original_compose.decode("ascii")
    except UnicodeError:
        fail("runtime snapshot source compose.env is not ASCII")
    expected_source = "".join(
        f"{key}={source_runtime[key]}\n" for key in RUNTIME_ENV_KEYS
    )
    if source_compose != expected_source:
        fail("runtime snapshot source compose.env changed during parsing")
    snapshot_values = dict(original)
    snapshot_values["compose.env"] = "".join(
        f"{key}={runtime[key]}\n" for key in RUNTIME_ENV_KEYS
    ).encode("ascii")
    # Re-read every source after the cross-file snapshot. A concurrent writer
    # cannot silently mix versions across the copied runtime inputs.
    for relative, limit in limits.items():
        if stable_source_bytes(source / relative, f"runtime snapshot source {relative}", limit) != original[relative]:
            fail("runtime snapshot source changed while the cross-file snapshot was created")
    (output / "config").mkdir(mode=0o700)
    (output / "secrets").mkdir(mode=0o700)
    for relative, value in snapshot_values.items():
        write_snapshot_file(output / relative, value)
    for relative, value in snapshot_values.items():
        if stable_source_bytes(output / relative, f"new runtime snapshot {relative}", limits[relative]) != value:
            fail("new runtime snapshot bytes differ from the stable source snapshot")
    for path in (output / "config", output / "secrets", output):
        path.chmod(0o500)
    for relative in snapshot_values:
        (output / relative).chmod(0o400)


def runtime_input_anchor(path: Path) -> dict[str, str]:
    path = canonical_regular(path, "pre-mutation runtime input anchor")
    descriptor = -1
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        info = os.fstat(descriptor)
    except OSError as exc:
        fail(f"cannot open pre-mutation runtime input anchor: {exc}")
    try:
        if (
            not stat.S_ISREG(info.st_mode)
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_uid != os.getuid()
            or info.st_gid != os.getgid()
            or info.st_nlink != 1
            or info.st_size < 1
            or info.st_size > 4096
        ):
            fail("pre-mutation runtime input anchor metadata is invalid")
        raw = bytearray()
        while True:
            chunk = os.read(descriptor, 4096)
            if not chunk:
                break
            raw.extend(chunk)
            if len(raw) > 4096:
                fail("pre-mutation runtime input anchor grew beyond its size limit while read")
        after = os.fstat(descriptor)
        stable_fields = (
            "st_dev", "st_ino", "st_mode", "st_uid", "st_gid", "st_nlink",
            "st_size", "st_mtime_ns", "st_ctime_ns",
        )
        if len(raw) != info.st_size or any(
            getattr(info, field) != getattr(after, field) for field in stable_fields
        ):
            fail("pre-mutation runtime input anchor changed while read")
    finally:
        os.close(descriptor)
    try:
        text = bytes(raw).decode("ascii")
        value = json.loads(text, object_pairs_hook=no_duplicate_keys)
    except (UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"pre-mutation runtime input anchor is invalid: {exc}")
    if (
        not text.endswith("\n")
        or text != json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n"
        or not isinstance(value, dict)
        or set(value) != set(RUNTIME_INPUT_FILES)
        or any(not isinstance(item, str) or HEX64.fullmatch(item) is None for item in value.values())
    ):
        fail("pre-mutation runtime input anchor schema is invalid")
    return value


def write_runtime_input_anchor(runtime_root: str, output: Path) -> None:
    if not output.is_absolute() or output.name != RUNTIME_INPUT_ANCHOR:
        fail("runtime input anchor output must use the fixed absolute basename")
    parent = output.parent
    try:
        parent_info = parent.stat()
        physical_parent = parent.resolve(strict=True)
    except OSError as exc:
        fail(f"cannot resolve runtime input anchor parent: {exc}")
    if (
        parent.is_symlink()
        or physical_parent != parent
        or not stat.S_ISDIR(parent_info.st_mode)
        or stat.S_IMODE(parent_info.st_mode) != 0o700
        or parent_info.st_uid != os.getuid()
        or parent_info.st_gid != os.getgid()
    ):
        fail("runtime input anchor parent must be a physical private directory")
    input_values = runtime_input_values(runtime_root)
    input_digests = {
        logical: hashlib.sha256(value).hexdigest()
        for logical, value in input_values.items()
    }
    payload = (
        json.dumps(input_digests, sort_keys=True, separators=(",", ":"))
        + "\n"
    ).encode("ascii")
    descriptor = -1
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(output, flags, 0o600)
        info = os.fstat(descriptor)
        if (
            not stat.S_ISREG(info.st_mode)
            or stat.S_IMODE(info.st_mode) != 0o600
            or info.st_uid != os.getuid()
            or info.st_gid != os.getgid()
            or info.st_nlink != 1
        ):
            fail("new runtime input anchor metadata is invalid")
        view = memoryview(payload)
        while view:
            written = os.write(descriptor, view)
            if written <= 0:
                fail("cannot write pre-mutation runtime input anchor")
            view = view[written:]
        os.fsync(descriptor)
    except OSError as exc:
        fail(f"cannot create pre-mutation runtime input anchor: {exc}")
    finally:
        if descriptor >= 0:
            os.close(descriptor)
    for logical, evidence_name in RUNTIME_INPUT_EVIDENCE_FILES.items():
        write_snapshot_file(output.parent / evidence_name, input_values[logical])
    if runtime_input_anchor(output) != evidence_runtime_input_digests(output.parent):
        fail("recorded runtime input bytes differ from the pre-mutation anchor")
    if runtime_input_anchor(output) != runtime_input_digests(runtime_root):
        fail("runtime inputs changed while the pre-mutation anchor was created")


def result_values(path: Path) -> dict[str, str]:
    try:
        text = path.read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read result.txt: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail("result.txt is not canonical ASCII")
    result: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail("result.txt contains a malformed line")
        key, value = line.split("=", 1)
        if key in result or not re.fullmatch(r"[a-z_]+", key) or "\n" in value:
            fail("result.txt contains a duplicate/unsafe field")
        result[key] = value
    if result.get("status") not in {"completed", "failed"} or result.get("action") not in {"start-edge", "stop-edge"}:
        fail("result status/action is invalid")
    if result["status"] == "completed":
        if set(result) != {
            "status", "action", "evidence_status", "rollback_status", "coturn_status",
        }:
            fail("completed result.txt schema changed")
        expected = {
            "evidence_status": "complete",
            "rollback_status": "not-required" if result["action"] == "start-edge" else "not-applicable",
            "coturn_status": "exact-pre-action-fingerprint-verified",
        }
        if any(result[key] != value for key, value in expected.items()):
            fail("completed result.txt makes an unsupported completion claim")
    else:
        if set(result) != {
            "status", "action", "reason", "collection_status", "rollback_status",
            "evidence_status", "coturn_status",
        }:
            fail("failed result.txt schema changed")
        if not result["reason"] or len(result["reason"].encode("ascii")) > 1024:
            fail("failed result reason is empty or unbounded")
        if result["collection_status"] not in {"complete", "incomplete"}:
            fail("failed collection status is invalid")
        if result["evidence_status"] not in {"complete", "incomplete"}:
            fail("failed evidence status is invalid")
        if result["action"] == "start-edge":
            if result["rollback_status"] not in {
                "completed", "incomplete", "not-proven-after-evidence-erasure",
            }:
                fail("failed start-edge rollback status is invalid")
        elif result["rollback_status"] != "not-applicable":
            fail("failed stop-edge rollback status is invalid")
        if result["coturn_status"] not in {
            "exact-pre-action-fingerprint-verified", "not-proven",
            "current-facts-collected-but-unchanged-not-proven",
        }:
            fail("failed coturn status is invalid")
    return result


def artifacts(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in root.iterdir():
        if path.name == "action.json":
            continue
        if path.is_symlink() or not path.is_file() or Path(path.name).name != path.name:
            fail("action evidence contains a non-regular/unsafe artifact")
        result[path.name] = digest(path)
    if "result.txt" not in result:
        fail("action evidence lacks result.txt")
    return dict(sorted(result.items()))


def validate_artifact_contract(result: dict[str, str], artifact_hashes: dict[str, str]) -> None:
    names = set(artifact_hashes)
    if result["status"] == "completed":
        if names != COMPLETED_ARTIFACTS[result["action"]]:
            fail("completed action evidence artifact set is incomplete or expanded")
        return
    if not names <= FAILED_ARTIFACTS or not {
        "result.txt", "failure-detail.txt", RUNTIME_INPUT_ANCHOR,
    } <= names:
        fail("failed action evidence artifact set is incomplete or expanded")
    if result["rollback_status"] == "completed":
        required = {
            "pre-inspect.json", "pre-summary.json", "rollback-pre-network-remove.json",
            "rollback-pre-network-remove-summary.json", "rollback-inspect.json",
            "rollback-summary.json",
        }
        if result["action"] != "start-edge" or not required <= names:
            fail("verified rollback claim lacks its complete state evidence")


def run_gate(command: list[str], label: str) -> bytes:
    try:
        completed = subprocess.run(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            env={
                "PATH": os.environ.get("PATH", os.defpath),
                "PYTHONDONTWRITEBYTECODE": "1",
            },
        )
    except OSError as exc:
        fail(f"cannot execute {label}: {exc}")
    if completed.returncode != 0:
        fail(f"{label} rejected the evidence")
    return completed.stdout


def replay_state(
    evidence_root: Path, manifest: dict[str, object], runtime: dict[str, str], compose: Path,
    phase: str, state_path: Path, summary_path: Path, baseline: Path | None = None,
) -> None:
    runtime_root = Path(str(manifest["runtime_root"]))
    state = canonical_regular(state_path, f"{phase} state evidence")
    summary = canonical_regular(summary_path, f"{phase} summary evidence")
    verifier = canonical_regular(Path(__file__).with_name("verify-runtime-state.py"), "runtime state verifier")
    command = [
        sys.executable, "-B", "-I", str(verifier), "--phase", phase,
        "--edge-image", runtime["DJI4G_EDGE_IMAGE"],
        "--cloudflared-image", runtime["DJI4G_CLOUDFLARED_IMAGE"],
        "--coturn-image", runtime["DJI4G_COTURN_IMAGE"],
        "--uid-gid", f'{runtime["DJI4G_EDGE_UID"]}:{runtime["DJI4G_EDGE_GID"]}',
        "--runtime-root", str(runtime_root), "--compose-file", str(compose),
        "--edge-env-evidence", str(canonical_regular(
            evidence_root / RUNTIME_INPUT_EVIDENCE_FILES["config/edge.env"],
            "recorded runtime edge.env",
        )),
    ]
    if baseline is not None:
        command.extend(("--baseline", str(canonical_regular(baseline, f"{phase} baseline evidence"))))
    command.append(str(state))
    output = run_gate(command, f"runtime state replay ({phase})")
    if output != summary.read_bytes():
        fail(f"stored {phase} summary differs from a fresh verifier replay")


def verify_completed_or_rollback_state(
    root: Path, manifest: dict[str, object], result: dict[str, str], compose: Path,
) -> None:
    recorded_inputs = evidence_runtime_input_values(root)
    runtime = parse_runtime_environment(
        recorded_inputs["compose.env"], str(manifest["runtime_root"]),
        "recorded runtime compose.env",
    )
    images = manifest["images"]
    if not isinstance(images, dict):
        fail("action image identity schema changed")
    if (
        runtime["DJI4G_EDGE_IMAGE"] != images.get("edge")
        or runtime["DJI4G_CLOUDFLARED_IMAGE"] != images.get("cloudflared")
        or f'{runtime["DJI4G_EDGE_UID"]}:{runtime["DJI4G_EDGE_GID"]}' != manifest["runtime_uid_gid"]
    ):
        fail("action identity differs from the physical runtime configuration")
    if manifest.get("runtime_inputs_sha256") != evidence_runtime_input_digests(root):
        fail("recorded runtime identity/authorization bytes differ from the action")
    live_root = Path(str(manifest["runtime_root"]))
    if live_root.exists() or live_root.is_symlink():
        if live_root.is_symlink() or manifest.get("runtime_inputs_sha256") != runtime_input_digests(str(live_root)):
            fail("live immutable runtime snapshot differs from the recorded action inputs")
    pre = root / "pre-inspect.json"
    if result["status"] == "completed" or result["rollback_status"] == "completed":
        replay_state(root, manifest, runtime, compose, "pre-start-edge" if result["action"] == "start-edge" else "pre-stop-edge", pre, root / "pre-summary.json")
    if result["status"] == "completed" and result["action"] == "start-edge":
        replay_state(root, manifest, runtime, compose, "post-edge", root / "post-inspect.json", root / "post-summary.json", pre)
    elif result["status"] == "completed" and result["action"] == "stop-edge":
        replay_state(root, manifest, runtime, compose, "pre-control-cleanup-stop", root / "pre-network-remove.json", root / "pre-network-remove-summary.json", pre)
        replay_state(root, manifest, runtime, compose, "post-stop-edge", root / "post-inspect.json", root / "post-summary.json", pre)
    elif result["rollback_status"] == "completed":
        replay_state(root, manifest, runtime, compose, "pre-control-cleanup-rollback", root / "rollback-pre-network-remove.json", root / "rollback-pre-network-remove-summary.json", pre)
        replay_state(root, manifest, runtime, compose, "post-rollback-edge", root / "rollback-inspect.json", root / "rollback-summary.json", pre)


def verify_source_and_image_evidence(args: argparse.Namespace, manifest: dict[str, object], result: dict[str, str]) -> None:
    try:
        repo_root = args.repo_root.resolve(strict=True)
    except OSError as exc:
        fail(f"cannot resolve repository root: {exc}")
    if args.repo_root.is_symlink() or not repo_root.is_dir() or repo_root != args.repo_root:
        fail("repository root must be a physical canonical directory")
    source_verifier = canonical_regular(Path(__file__).parent / "image/verify-source.py", "source verifier")
    run_gate([
        sys.executable, "-B", "-I", str(source_verifier), "--repo-root", str(repo_root),
        "--lock", str(args.source_lock),
    ], "source provenance verifier")
    if result["action"] != "start-edge":
        return
    image_verifier = canonical_regular(Path(__file__).parent / "image/verify-evidence.py", "image evidence verifier")
    run_gate([
        sys.executable, "-B", "-I", str(image_verifier), "--evidence", str(args.edge_evidence),
        "--image", str(manifest["images"]["edge"]), "--repo-root", str(repo_root),
        "--trusted-source-lock", str(args.source_lock),
        "--trusted-toolchain-lock", str(args.toolchain_lock),
        "--as-of", str(manifest["completed_at"]),
    ], "edge image evidence verifier")


def coturn_baseline(root: Path) -> dict[str, object]:
    path = root / "pre-inspect.json"
    if not path.exists():
        return {"status": "unavailable"}
    if path.is_symlink() or not path.is_file():
        fail("pre-action coturn evidence is not a regular file")
    try:
        value = json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=no_duplicate_keys
        )
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"pre-action coturn evidence is invalid: {exc}")
    containers = value.get("containers") if isinstance(value, dict) else None
    if not isinstance(containers, list):
        fail("pre-action coturn container evidence is invalid")
    matches: list[dict] = []
    for container in containers:
        config = container.get("Config") if isinstance(container, dict) else None
        labels = config.get("Labels") if isinstance(config, dict) else None
        if isinstance(labels, dict) and labels.get("com.docker.compose.project") == "dji4g-public-edge" and labels.get("com.docker.compose.service") == "coturn":
            matches.append(container)
    if not matches:
        return {"status": "absent"}
    if len(matches) != 1:
        fail("pre-action coturn evidence is ambiguous")
    container = matches[0]
    state = container.get("State")
    restart = container.get("RestartCount")
    identifier = container.get("Id")
    image = container.get("Image")
    if (
        not isinstance(identifier, str) or CONTAINER_ID.fullmatch(identifier) is None
        or not isinstance(image, str) or TARGET_ID.fullmatch(image) is None
        or isinstance(restart, bool) or not isinstance(restart, int) or restart < 0
        or not isinstance(state, dict)
        or not isinstance(state.get("StartedAt"), str)
        or not isinstance(state.get("Status"), str)
        or not isinstance(state.get("Running"), bool)
    ):
        fail("pre-action coturn identity/state evidence is incomplete")
    return {
        "status": "present",
        "container_id": identifier,
        "image_id": image,
        "started_at": state["StartedAt"],
        "restart_count": restart,
        "runtime_status": state["Status"],
        "running": state["Running"],
    }


def create(args: argparse.Namespace) -> int:
    root = args.evidence
    if root.is_symlink() or not root.is_dir() or (root / "action.json").exists():
        fail("action evidence stage/action.json is invalid")
    result = result_values(root / "result.txt")
    configured_root = Path(args.configured_runtime_root)
    runtime_root = Path(args.runtime_root)
    if (
        not configured_root.is_absolute() or configured_root.is_symlink()
        or not configured_root.is_dir() or configured_root.resolve(strict=True) != configured_root
    ):
        fail("configured runtime root is not a physical canonical directory")
    valid_runtime_relationship = (
        valid_snapshot_path(configured_root, runtime_root)
        if result["action"] == "start-edge" else runtime_root == configured_root
    )
    if not valid_runtime_relationship:
        fail("action runtime input root does not match its configured runtime/action contract")
    completed = dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    edge_evidence_sha = None
    if args.edge_evidence is not None:
        evidence_manifest = args.edge_evidence / "evidence.json"
        if evidence_manifest.is_symlink() or not evidence_manifest.is_file():
            fail("verified edge evidence manifest is missing")
        edge_evidence_sha = digest(evidence_manifest)
    expected_anchor = root / RUNTIME_INPUT_ANCHOR
    if args.runtime_input_anchor != expected_anchor:
        fail("pre-mutation runtime input anchor must be the staged fixed artifact")
    anchored_runtime_inputs = runtime_input_anchor(args.runtime_input_anchor)
    runtime = load_runtime_environment(args.runtime_root)
    if (
        f'{runtime["DJI4G_EDGE_UID"]}:{runtime["DJI4G_EDGE_GID"]}' != args.uid_gid
        or runtime["DJI4G_EDGE_IMAGE"] != args.edge_image
        or runtime["DJI4G_CLOUDFLARED_IMAGE"] != args.cloudflared_image
    ):
        fail("requested action identity differs from runtime compose.env")
    if runtime_input_digests(args.runtime_root) != anchored_runtime_inputs:
        fail("runtime identity/authorization inputs changed after the pre-mutation anchor")
    artifact_hashes = artifacts(root)
    validate_artifact_contract(result, artifact_hashes)
    if runtime_input_anchor(args.runtime_input_anchor) != anchored_runtime_inputs:
        fail("pre-mutation runtime input anchor changed during evidence creation")
    if runtime_input_digests(args.runtime_root) != anchored_runtime_inputs:
        fail("runtime identity/authorization inputs changed during evidence creation")
    manifest = {
        "format": 1,
        "action": result["action"],
        "status": result["status"],
        "completed_at": completed,
        "runtime_root": args.runtime_root,
        "configured_runtime_root": args.configured_runtime_root,
        "runtime_uid_gid": args.uid_gid,
        "runtime_inputs_sha256": anchored_runtime_inputs,
        "images": {
            "edge": args.edge_image,
            "cloudflared": args.cloudflared_image,
        },
        "coturn_baseline": coturn_baseline(root),
        "verified_edge_evidence_sha256": edge_evidence_sha,
        "source_lock_sha256": digest(args.source_lock),
        "toolchain_lock_sha256": digest(args.toolchain_lock),
        "compose_sha256": digest(args.compose),
        "result": result,
        "artifacts": artifact_hashes,
    }
    verify_completed_or_rollback_state(root, manifest, result, args.compose)
    (root / "action.json").write_text(
        json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="ascii",
    )
    return 0


def verify(args: argparse.Namespace) -> int:
    root = args.evidence
    path = root / "action.json"
    if root.is_symlink() or not root.is_dir() or path.is_symlink() or not path.is_file():
        fail("action evidence directory/manifest is invalid")
    try:
        value = json.loads(
            path.read_text(encoding="ascii"), object_pairs_hook=no_duplicate_keys
        )
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"action manifest is invalid: {exc}")
    expected_keys = {
        "format", "action", "status", "completed_at", "runtime_root", "configured_runtime_root", "runtime_uid_gid",
        "runtime_inputs_sha256", "images", "coturn_baseline", "verified_edge_evidence_sha256", "source_lock_sha256",
        "toolchain_lock_sha256", "compose_sha256", "result", "artifacts",
    }
    if not isinstance(value, dict) or set(value) != expected_keys or value.get("format") != 1:
        fail("action manifest schema changed")
    result = result_values(root / "result.txt")
    if value.get("result") != result or value.get("action") != result["action"] or value.get("status") != result["status"]:
        fail("action manifest and result.txt disagree")
    artifact_hashes = artifacts(root)
    if value.get("artifacts") != artifact_hashes:
        fail("action evidence artifact hashes changed")
    validate_artifact_contract(result, artifact_hashes)
    if any(not isinstance(value.get(key), str) or HEX64.fullmatch(value[key]) is None for key in (
        "source_lock_sha256", "toolchain_lock_sha256", "compose_sha256"
    )):
        fail("action provenance digests are invalid")
    runtime_inputs = value.get("runtime_inputs_sha256")
    if (
        not isinstance(runtime_inputs, dict)
        or set(runtime_inputs) != set(RUNTIME_INPUT_FILES)
        or any(not isinstance(item, str) or HEX64.fullmatch(item) is None for item in runtime_inputs.values())
    ):
        fail("action runtime input digest schema is invalid")
    if runtime_inputs != runtime_input_anchor(root / RUNTIME_INPUT_ANCHOR):
        fail("action manifest no longer matches its pre-mutation runtime input anchor")
    edge_digest = value.get("verified_edge_evidence_sha256")
    if edge_digest is not None and (not isinstance(edge_digest, str) or HEX64.fullmatch(edge_digest) is None):
        fail("action image-evidence digest is invalid")
    runtime_root = value.get("runtime_root")
    if (
        not isinstance(runtime_root, str)
        or not runtime_root.startswith("/")
        or "//" in runtime_root
        or "/../" in runtime_root
        or runtime_root.endswith(("/", "/.", "/.."))
    ):
        fail("action runtime root is not a canonical absolute path")
    configured_runtime_root = value.get("configured_runtime_root")
    if (
        not isinstance(configured_runtime_root, str)
        or not configured_runtime_root.startswith("/")
        or "//" in configured_runtime_root
        or "/../" in configured_runtime_root
        or configured_runtime_root.endswith(("/", "/.", "/.."))
    ):
        fail("configured runtime root is not a canonical absolute path")
    valid_runtime_relationship = (
        valid_snapshot_path(Path(configured_runtime_root), Path(runtime_root))
        if result["action"] == "start-edge" else runtime_root == configured_runtime_root
    )
    if not valid_runtime_relationship:
        fail("action runtime/configured root relationship changed")
    if not isinstance(value.get("runtime_uid_gid"), str) or re.fullmatch(
        r"[1-9][0-9]*:[1-9][0-9]*", value["runtime_uid_gid"]
    ) is None:
        fail("action runtime UID:GID is invalid")
    images = value.get("images")
    if not isinstance(images, dict) or set(images) != {"edge", "cloudflared"}:
        fail("action image identity schema changed")
    if any(not isinstance(image, str) or IMAGE.fullmatch(image) is None for image in images.values()):
        fail("action contains a mutable/invalid image identity")
    if value.get("coturn_baseline") != coturn_baseline(root):
        fail("action coturn baseline differs from pre-action evidence")
    if result["action"] == "start-edge" and edge_digest is None:
        fail("start-edge action lacks verified image-evidence identity")
    if result["action"] == "stop-edge" and edge_digest is not None:
        fail("stop-edge unexpectedly claims a fresh image audit")
    try:
        timestamp = dt.datetime.fromisoformat(str(value["completed_at"]).replace("Z", "+00:00"))
    except ValueError:
        fail("action completion timestamp is invalid")
    if timestamp.tzinfo is None:
        fail("action completion timestamp lacks a timezone")
    if timestamp > dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=5):
        fail("action completion timestamp is in the future")
    external = {
        "source_lock_sha256": args.source_lock,
        "toolchain_lock_sha256": args.toolchain_lock,
        "compose_sha256": args.compose,
    }
    for key, external_path in external.items():
        if external_path.is_symlink() or not external_path.is_file():
            fail(f"external trust anchor is missing/symlinked: {external_path.name}")
        if value[key] != digest(external_path):
            fail(f"action provenance differs from external trust anchor: {key}")
    canonical_regular(args.compose, "external Compose file")
    if result["action"] == "start-edge":
        if args.edge_evidence is None:
            fail("start-edge verification requires --edge-evidence")
        evidence_manifest = args.edge_evidence / "evidence.json"
        if args.edge_evidence.is_symlink() or not args.edge_evidence.is_dir() or evidence_manifest.is_symlink() or not evidence_manifest.is_file():
            fail("external edge image evidence is missing/symlinked")
        if edge_digest != digest(evidence_manifest):
            fail("action image evidence differs from the external audited manifest")
    elif args.edge_evidence is not None:
        fail("stop-edge verification does not accept --edge-evidence")
    verify_completed_or_rollback_state(root, value, result, args.compose)
    verify_source_and_image_evidence(args, value, result)
    print(json.dumps({"format": 1, "action": value["action"], "status": value["status"]}, sort_keys=True, separators=(",", ":")))
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="mode", required=True)
    snapshot = subparsers.add_parser("snapshot")
    snapshot.add_argument("--source-root", required=True)
    snapshot.add_argument("--declared-root", required=True)
    snapshot.add_argument("--output", required=True, type=Path)
    anchor = subparsers.add_parser("anchor")
    anchor.add_argument("--runtime-root", required=True)
    anchor.add_argument("--output", required=True, type=Path)
    creator = subparsers.add_parser("create")
    creator.add_argument("--evidence", required=True, type=Path)
    creator.add_argument("--runtime-root", required=True)
    creator.add_argument("--configured-runtime-root", required=True)
    creator.add_argument("--uid-gid", required=True)
    creator.add_argument("--edge-image", required=True)
    creator.add_argument("--cloudflared-image", required=True)
    creator.add_argument("--runtime-input-anchor", required=True, type=Path)
    creator.add_argument("--edge-evidence", type=Path)
    creator.add_argument("--source-lock", required=True, type=Path)
    creator.add_argument("--toolchain-lock", required=True, type=Path)
    creator.add_argument("--compose", required=True, type=Path)
    verifier = subparsers.add_parser("verify")
    verifier.add_argument("--evidence", required=True, type=Path)
    verifier.add_argument("--source-lock", required=True, type=Path)
    verifier.add_argument("--toolchain-lock", required=True, type=Path)
    verifier.add_argument("--compose", required=True, type=Path)
    verifier.add_argument("--edge-evidence", type=Path)
    verifier.add_argument("--repo-root", required=True, type=Path)
    args = parser.parse_args()
    if args.mode == "snapshot":
        snapshot_runtime(args.source_root, args.declared_root, args.output)
        return 0
    if args.mode == "anchor":
        write_runtime_input_anchor(args.runtime_root, args.output)
        return 0
    return create(args) if args.mode == "create" else verify(args)


if __name__ == "__main__":
    raise SystemExit(main())
