#!/usr/bin/env python3
"""Verify an edge build bundle, including the Docker archive/config identity."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import importlib.util
import json
import re
import sys
import tarfile
from pathlib import Path, PurePosixPath


IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}")
HEX64 = re.compile(r"[0-9a-f]{64}")
LOCAL_RETENTION_REF = re.compile(
    r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?"
    r"(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+"
    r":dji4g-build-[0-9a-f]{16}-[0-9a-f]{32}-a"
)
BUILD_ARTIFACTS = {
    "edge-image-reference.txt",
    "local-image-retention-reference.txt",
    "image-inspect.json",
    "image.tar",
    "image.tar.sha256",
    "source.lock",
    "toolchain.lock",
}
TOOLCHAIN_KEYS = (
    "FORMAT",
    "TARGET_PLATFORM",
    "DOCKER_SERVER_VERSION",
    "DOCKER_CLIENT_VERSION",
    "IMAGE_STORE_DRIVER",
    "DOCKER_ROOT_CONFIG_DIR",
    "DOCKER_ROOT_CONFIG_SHA256",
    "COMPOSE_CLI_VERSION",
    "COMPOSE_PACKAGE_NAME",
    "COMPOSE_PACKAGE_VERSION",
    "COMPOSE_PLUGIN_PATH",
    "COMPOSE_LINUX_AMD64_SHA256",
    "COMPOSE_STATIC_DARWIN_ARM64_VERSION",
    "COMPOSE_STATIC_DARWIN_ARM64_SHA256",
    "CLOUDFLARED_IMAGE",
    "BUILDX_VERSION",
    "BUILDX_PLUGIN_PATH",
    "BUILDX_LINUX_AMD64_SHA256",
    "DOCKERFILE_FRONTEND",
    "GO_BUILDER_IMAGE",
    "GO_BUILDER_TARGET_DIGEST",
    "GO_VERSION",
    "GOPROXY",
    "GOSUMDB",
    "SYFT_IMAGE",
    "SYFT_VERSION",
    "GRYPE_IMAGE",
    "GRYPE_VERSION",
    "GRYPE_DB_SCHEMA_VERSION",
    "GRYPE_DB_MAX_AGE_HOURS",
)
OCI_LAYOUT_MEDIA_TYPE = "application/vnd.oci.image.index.v1+json"
INDEX_MEDIA_TYPES = {
    OCI_LAYOUT_MEDIA_TYPE,
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
MANIFEST_MEDIA_TYPES = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}
CONFIG_MEDIA_TYPES = {
    "application/vnd.oci.image.config.v1+json",
    "application/vnd.docker.container.image.v1+json",
}
# The trusted host Python standard library provides gzip but no zstd decoder;
# host tools are a documented trust boundary rather than a hash-locked input.
# decoder.  Accept only distributable gzip layers and fail closed on zstd,
# identity, foreign/nondistributable, or future media types.
GZIP_LAYER_MEDIA_TYPES = {
    "application/vnd.oci.image.layer.v1.tar+gzip",
    "application/vnd.docker.image.rootfs.diff.tar.gzip",
}
MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
MAX_LAYER_UNCOMPRESSED = 512 * 1024 * 1024
MAX_DESCRIPTOR_NODES = 128


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError as exc:
        fail(f"cannot hash {path.name}: {exc}")
    return digest.hexdigest()


def no_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=no_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"invalid JSON artifact {path.name}: {exc}")


def read_canonical_lock(path: Path, keys: tuple[str, ...]) -> dict[str, str]:
    try:
        text = path.read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read {path.name}: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail(f"{path.name} is not canonical ASCII lines")
    values: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail(f"{path.name} contains a malformed line")
        key, value = line.split("=", 1)
        if key in values:
            fail(f"{path.name} contains duplicate key {key}")
        values[key] = value
    if tuple(values) != keys or values.get("FORMAT") != "1":
        fail(f"{path.name} schema or key order changed")
    return values


def load_source_verifier():
    path = Path(__file__).with_name("verify-source.py")
    sys.dont_write_bytecode = True
    spec = importlib.util.spec_from_file_location("public_edge_verify_source", path)
    if spec is None or spec.loader is None:
        fail("cannot load verify-source.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def verify_trusted_source(repo_root: Path, source_lock: Path) -> dict[str, str]:
    verifier = load_source_verifier()
    lock = verifier.load_lock(source_lock)
    files = verifier.selected_files(repo_root.resolve())
    verifier.verify_repository_binding(repo_root.resolve(), lock, files)
    if len(files) != int(lock["SOURCE_FILE_COUNT"]):
        fail("trusted source lock file count differs from the selected commit tree")
    if verifier.tree_digest(repo_root.resolve(), files) != lock["SOURCE_TREE_SHA256"]:
        fail("trusted source lock digest differs from the selected commit tree")
    return lock


def safe_tar_name(name: str) -> bool:
    if not name or "\\" in name or "\x00" in name:
        return False
    path = PurePosixPath(name)
    return not path.is_absolute() and all(part not in ("", ".", "..") for part in path.parts)


def read_tar_file(archive: tarfile.TarFile, member: tarfile.TarInfo, maximum: int) -> bytes:
    if not member.isfile() or member.size < 0 or member.size > maximum:
        fail(f"Docker archive member has an invalid type/size: {member.name}")
    handle = archive.extractfile(member)
    if handle is None:
        fail(f"cannot read Docker archive member: {member.name}")
    data = handle.read(maximum + 1)
    if len(data) != member.size or len(data) > maximum:
        fail(f"Docker archive member size changed while reading: {member.name}")
    return data


def hash_tar_file(archive: tarfile.TarFile, member: tarfile.TarInfo, maximum: int) -> str:
    """Hash the exact raw bytes of one regular outer-archive member."""
    if not member.isfile() or member.size < 0 or member.size > maximum:
        fail(f"Docker archive member has an invalid type/size: {member.name}")
    handle = archive.extractfile(member)
    if handle is None:
        fail(f"cannot read Docker archive member: {member.name}")
    digest = hashlib.sha256()
    remaining = member.size
    while remaining:
        chunk = handle.read(min(1024 * 1024, remaining))
        if not chunk:
            fail(f"Docker archive member ended early: {member.name}")
        digest.update(chunk)
        remaining -= len(chunk)
    if handle.read(1) != b"":
        fail(f"Docker archive member exceeds its declared size: {member.name}")
    return "sha256:" + digest.hexdigest()


def parse_json_bytes(data: bytes, context: str) -> object:
    try:
        return json.loads(data, object_pairs_hook=no_duplicate_keys)
    except (UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"{context} JSON is invalid: {exc}")


def descriptor_blob(
    archive: tarfile.TarFile,
    by_name: dict[str, tarfile.TarInfo],
    descriptor: object,
    context: str,
    maximum: int,
) -> tuple[str, str, bytes]:
    """Validate one OCI descriptor and bind it to its exact raw blob bytes."""
    if not isinstance(descriptor, dict):
        fail(f"{context} descriptor is not an object")
    allowed = {"mediaType", "digest", "size", "urls", "annotations", "platform", "artifactType"}
    if set(descriptor) - allowed or "data" in descriptor:
        fail(f"{context} descriptor contains an unsupported/embedded field")
    # This archive is an image-content evidence artifact, not a naming/import
    # vehicle. Descriptor annotations can carry containerd image/ref names and
    # make `docker load` overwrite an unrelated local tag.
    if descriptor.get("annotations") not in (None, {}):
        fail(f"{context} descriptor annotations are forbidden")
    if descriptor.get("urls") not in (None, []) or descriptor.get("artifactType") not in (None, ""):
        fail(f"{context} descriptor carries an external/artifact reference")
    media_type = descriptor.get("mediaType")
    digest = descriptor.get("digest")
    size = descriptor.get("size")
    if not isinstance(media_type, str) or not media_type:
        fail(f"{context} descriptor mediaType is missing")
    if not isinstance(digest, str) or IMAGE_ID.fullmatch(digest) is None:
        fail(f"{context} descriptor digest is not canonical sha256")
    if isinstance(size, bool) or not isinstance(size, int) or size < 1 or size > maximum:
        fail(f"{context} descriptor size is invalid")
    name = "blobs/sha256/" + digest.removeprefix("sha256:")
    member = by_name.get(name)
    if member is None or not member.isfile():
        fail(f"{context} descriptor blob is missing: {name}")
    if member.size != size:
        fail(f"{context} descriptor size does not equal the raw blob size")
    if hash_tar_file(archive, member, maximum) != digest:
        fail(f"{context} descriptor digest does not equal the raw blob bytes")
    return media_type, name, read_tar_file(archive, member, maximum)


def collect_image_manifests(
    archive: tarfile.TarFile,
    by_name: dict[str, tarfile.TarInfo],
    descriptors: object,
) -> tuple[str, list[tuple[dict, dict]], set[str]]:
    """Traverse a bounded OCI index graph and return exact image manifests."""
    if not isinstance(descriptors, list) or len(descriptors) != 1:
        fail("OCI image index must contain exactly one exported target")
    target = descriptors[0]
    if not isinstance(target, dict) or not isinstance(target.get("digest"), str):
        fail("OCI exported target descriptor is invalid")
    target_digest = target["digest"]
    pending: list[tuple[object, int]] = [(item, 0) for item in descriptors]
    seen: set[str] = set()
    result: list[tuple[dict, dict]] = []
    reachable_names: set[str] = set()
    while pending:
        if len(seen) >= MAX_DESCRIPTOR_NODES:
            fail("OCI descriptor graph is unbounded")
        descriptor, depth = pending.pop(0)
        if depth > 4:
            fail("OCI descriptor graph nesting is unbounded")
        media_type, blob_name, data = descriptor_blob(
            archive, by_name, descriptor, "OCI image/index", 16 * 1024 * 1024
        )
        assert isinstance(descriptor, dict)
        digest = descriptor["digest"]
        if digest in seen:
            fail("OCI descriptor graph repeats a blob")
        seen.add(digest)
        reachable_names.add(blob_name)
        platform = descriptor.get("platform")
        if platform is not None:
            if not isinstance(platform, dict):
                fail("OCI platform descriptor is not an object")
            os_name = platform.get("os")
            architecture = platform.get("architecture")
            if os_name is not None and os_name != "linux":
                fail("OCI descriptor platform is not linux")
            if architecture is not None and architecture != "amd64":
                fail("OCI descriptor platform is not amd64")
        document = parse_json_bytes(data, "OCI descriptor")
        if not isinstance(document, dict) or document.get("schemaVersion") != 2:
            fail("OCI descriptor document schema is invalid")
        if document.get("mediaType") not in (None, media_type):
            fail("OCI descriptor mediaType disagrees with its document")
        if media_type in INDEX_MEDIA_TYPES:
            children = document.get("manifests")
            if not isinstance(children, list) or not children:
                fail("nested OCI image index has no manifests")
            pending.extend((child, depth + 1) for child in children)
        elif media_type in MANIFEST_MEDIA_TYPES:
            result.append((descriptor, document))
        else:
            fail(f"unsupported OCI index descriptor mediaType: {media_type}")
    if len(result) != 1:
        fail("OCI archive must resolve to exactly one image manifest")
    return target_digest, result, reachable_names


def gzip_layer_diff_id(
    archive: tarfile.TarFile,
    member: tarfile.TarInfo,
    media_type: str,
) -> str:
    if media_type not in GZIP_LAYER_MEDIA_TYPES:
        fail(f"unsupported layer compression/mediaType (gzip is required): {media_type}")
    raw = archive.extractfile(member)
    if raw is None:
        fail(f"cannot read compressed layer blob: {member.name}")
    digest = hashlib.sha256()
    total = 0
    try:
        with gzip.GzipFile(fileobj=raw, mode="rb") as decoded:
            while True:
                chunk = decoded.read(1024 * 1024)
                if not chunk:
                    break
                total += len(chunk)
                if total > MAX_LAYER_UNCOMPRESSED:
                    fail("gzip layer uncompressed content is unbounded")
                digest.update(chunk)
    except (OSError, EOFError, gzip.BadGzipFile) as exc:
        fail(f"gzip layer cannot be decoded safely: {exc}")
    if total < 1:
        fail("gzip layer has no uncompressed tar bytes")
    return "sha256:" + digest.hexdigest()


def expected_labels(
    source: dict[str, str], source_lock_sha: str, toolchain_lock_sha: str
) -> dict[str, str]:
    return {
        "org.opencontainers.image.title": "DJOneHub public edge",
        "org.opencontainers.image.description": "Fail-closed read-only public control edge",
        "org.opencontainers.image.source": "https://github.com/example/maccellular",
        "org.opencontainers.image.revision": source["SOURCE_REVISION"],
        "org.opencontainers.image.created": source["SOURCE_CREATED_RFC3339"],
        "io.maccellular.source-tree-sha256": source["SOURCE_TREE_SHA256"],
        "io.maccellular.source-lock-sha256": source_lock_sha,
        "io.maccellular.toolchain-lock-sha256": toolchain_lock_sha,
        "io.maccellular.runtime": "scratch-no-shell",
        "io.maccellular.surface": "public-read-only",
    }


def validate_runtime_config(config: object, labels: dict[str, str], context: str) -> dict:
    if not isinstance(config, dict):
        fail(f"{context} runtime config is not an object")
    allowed_keys = {
        "Hostname", "Domainname", "User", "AttachStdin", "AttachStdout",
        "AttachStderr", "ExposedPorts", "Tty", "OpenStdin", "StdinOnce",
        "Env", "Cmd", "Healthcheck", "ArgsEscaped", "Image", "Volumes",
        "WorkingDir", "Entrypoint", "NetworkDisabled", "MacAddress", "OnBuild",
        "Labels", "StopSignal", "Shell",
    }
    extra = set(config) - allowed_keys
    if extra:
        fail(f"{context} runtime config contains an unapproved field: {sorted(extra)[0]}")
    if config.get("Entrypoint") != ["/djonehub-edge"] or config.get("Cmd") not in (None, []):
        fail(f"{context} entrypoint is invalid")
    if config.get("User") != "65532:65532":
        fail(f"{context} default user is invalid")
    if config.get("ExposedPorts") != {"8080/tcp": {}}:
        fail(f"{context} exposed-port declaration is invalid")
    if config.get("StopSignal") != "SIGTERM":
        fail(f"{context} stop signal is invalid")
    if config.get("Volumes") is not None:
        fail(f"{context} image-declared volumes are forbidden")
    if config.get("Env") not in (None, []):
        fail(f"{context} image environment is not empty")
    if config.get("WorkingDir") not in (None, "") or config.get("Image") not in (None, ""):
        fail(f"{context} working directory/parent image metadata is invalid")
    if config.get("OnBuild") not in (None, []) or config.get("Shell") not in (None, []):
        fail(f"{context} shell/on-build metadata is forbidden")
    if config.get("NetworkDisabled") not in (None, False):
        fail(f"{context} network metadata is invalid")
    for key in ("Hostname", "Domainname", "MacAddress"):
        if config.get(key) not in (None, ""):
            fail(f"{context} {key} metadata is invalid")
    for key in ("AttachStdin", "AttachStdout", "AttachStderr", "Tty", "OpenStdin", "StdinOnce"):
        value = config.get(key)
        if value not in (None, False) or isinstance(value, int) and not isinstance(value, bool):
            fail(f"{context} interactive flag is invalid: {key}")
    if "ArgsEscaped" in config and not isinstance(config["ArgsEscaped"], bool):
        fail(f"{context} ArgsEscaped has an invalid type")
    actual_labels = config.get("Labels")
    if actual_labels != labels:
        fail(f"{context} labels differ from the complete locked allowlist")
    health = config.get("Healthcheck")
    expected_health = {
        "Test": ["CMD", "/djonehub-edge-healthcheck"],
        "Interval": 10_000_000_000,
        "Timeout": 3_000_000_000,
        "StartPeriod": 5_000_000_000,
        "Retries": 3,
    }
    if health != expected_health:
        fail(f"{context} healthcheck is invalid")
    return {
        "Entrypoint": ["/djonehub-edge"],
        "Cmd": None,
        "User": "65532:65532",
        "ExposedPorts": {"8080/tcp": {}},
        "StopSignal": "SIGTERM",
        "Labels": labels,
        "Healthcheck": expected_health,
        "Volumes": None,
        "Env": None,
    }


def verify_archive(
    archive_path: Path,
    inspect_path: Path,
    image_id: str,
    source: dict[str, str],
    source_lock_sha: str,
    toolchain_lock_sha: str,
) -> None:
    if IMAGE_ID.fullmatch(image_id) is None:
        fail("external image ID is invalid")
    try:
        archive_size = archive_path.stat().st_size
    except OSError as exc:
        fail(f"cannot stat Docker archive: {exc}")
    if archive_path.is_symlink() or archive_size < 1 or archive_size > MAX_ARCHIVE_BYTES:
        fail("Docker archive is missing, symlinked, empty, or unbounded")

    try:
        with tarfile.open(archive_path, mode="r:*") as archive:
            members = archive.getmembers()
            if not members or len(members) > 10000:
                fail("Docker archive member count is invalid")
            by_name: dict[str, tarfile.TarInfo] = {}
            total_size = 0
            for member in members:
                if not safe_tar_name(member.name) or member.name in by_name:
                    fail(f"Docker archive contains an unsafe/duplicate member: {member.name!r}")
                if not (member.isfile() or member.isdir()):
                    fail(f"Docker archive contains a link/device member: {member.name}")
                total_size += max(member.size, 0)
                if total_size > MAX_ARCHIVE_BYTES:
                    fail("Docker archive declared content is unbounded")
                by_name[member.name] = member
            manifest_member = by_name.get("manifest.json")
            if manifest_member is None:
                fail("Docker archive lacks manifest.json")
            try:
                manifest = json.loads(
                    read_tar_file(archive, manifest_member, 1024 * 1024),
                    object_pairs_hook=no_duplicate_keys,
                )
            except (UnicodeError, json.JSONDecodeError, ValueError) as exc:
                fail(f"Docker archive manifest is invalid: {exc}")
            if not isinstance(manifest, list) or len(manifest) != 1 or not isinstance(manifest[0], dict):
                fail("Docker archive must describe exactly one image")
            entry = manifest[0]
            if set(entry) - {"Config", "RepoTags", "Layers", "LayerSources"}:
                fail("Docker archive manifest contains an unknown identity field")
            if entry.get("RepoTags") not in (None, []):
                fail("Docker archive compatibility manifest may not carry repository tags")
            if entry.get("LayerSources") not in (None, {}):
                fail("Docker archive compatibility manifest may not carry layer sources")
            config_name = entry.get("Config")
            layers = entry.get("Layers")
            if not isinstance(config_name, str) or not safe_tar_name(config_name):
                fail("Docker archive config path is invalid")
            if not isinstance(layers, list) or not layers or any(
                not isinstance(name, str) or not safe_tar_name(name) for name in layers
            ):
                fail("Docker archive layer list is invalid")
            if len(set(layers)) != len(layers) or config_name in layers:
                fail("Docker archive repeats an identity/layer member")

            layout_member = by_name.get("oci-layout")
            index_member = by_name.get("index.json")
            if layout_member is None or index_member is None:
                fail("Docker archive lacks the containerd OCI layout/index")
            layout = parse_json_bytes(
                read_tar_file(archive, layout_member, 1024), "OCI layout"
            )
            if layout != {"imageLayoutVersion": "1.0.0"}:
                fail("OCI layout version/schema is invalid")
            index = parse_json_bytes(
                read_tar_file(archive, index_member, 4 * 1024 * 1024), "OCI index"
            )
            if (
                not isinstance(index, dict)
                or index.get("schemaVersion") != 2
                or index.get("mediaType") != OCI_LAYOUT_MEDIA_TYPE
                or set(index) - {"schemaVersion", "mediaType", "manifests", "annotations"}
            ):
                fail("OCI top-level index schema/mediaType is invalid")
            if index.get("annotations") not in (None, {}):
                fail("OCI top-level index annotations are forbidden")
            target_image_id, image_manifests, reachable_names = collect_image_manifests(
                archive, by_name, index.get("manifests")
            )
            if target_image_id != image_id:
                fail("external image ID does not equal the exported OCI target descriptor")
            _, image_manifest = image_manifests[0]
            if set(image_manifest) - {
                "schemaVersion", "mediaType", "config", "layers", "annotations", "subject"
            }:
                fail("OCI image manifest contains an unsupported field")
            if image_manifest.get("annotations") not in (None, {}) or image_manifest.get("subject") is not None:
                fail("OCI image manifest annotations/subject are forbidden")
            config_descriptor = image_manifest.get("config")
            manifest_layers = image_manifest.get("layers")
            if not isinstance(manifest_layers, list) or not manifest_layers:
                fail("OCI image manifest has no layers")
            config_media_type, oci_config_name, config_bytes = descriptor_blob(
                archive, by_name, config_descriptor, "OCI image config", 4 * 1024 * 1024
            )
            if config_media_type not in CONFIG_MEDIA_TYPES:
                fail("OCI image config mediaType is unsupported")
            if oci_config_name != config_name:
                fail("Docker compatibility manifest config is not the OCI config descriptor")
            reachable_names.add(oci_config_name)

            descriptor_layers: list[tuple[str, str]] = []
            for position, descriptor in enumerate(manifest_layers):
                media_type, name, _ = descriptor_blob(
                    archive,
                    by_name,
                    descriptor,
                    f"OCI image layer {position}",
                    MAX_ARCHIVE_BYTES,
                )
                descriptor_layers.append((media_type, name))
                reachable_names.add(name)
            if [name for _, name in descriptor_layers] != layers:
                fail("Docker compatibility layer order differs from the OCI manifest")
            layer_diff_ids = [
                gzip_layer_diff_id(archive, by_name[name], media_type)
                for media_type, name in descriptor_layers
            ]
            file_names = {name for name, member in by_name.items() if member.isfile()}
            expected_files = {"oci-layout", "index.json", "manifest.json", *reachable_names}
            if file_names != expected_files:
                fail("Docker archive contains an extra, unreferenced, or missing file member")
            directory_names = {name.rstrip("/") for name, member in by_name.items() if member.isdir()}
            if directory_names - {"blobs", "blobs/sha256"}:
                fail("Docker archive contains an unexpected directory member")
    except (OSError, tarfile.TarError) as exc:
        fail(f"cannot parse Docker archive: {exc}")

    archive_config_digest = "sha256:" + hashlib.sha256(config_bytes).hexdigest()
    if archive_config_digest != config_descriptor["digest"]:
        fail("Docker archive config bytes do not equal the OCI config descriptor")
    try:
        archive_config = json.loads(config_bytes, object_pairs_hook=no_duplicate_keys)
    except (UnicodeError, json.JSONDecodeError, ValueError) as exc:
        fail(f"Docker archive config JSON is invalid: {exc}")
    if not isinstance(archive_config, dict):
        fail("Docker archive config JSON is not an object")
    if set(archive_config) - {
        "created", "author", "architecture", "os", "os.version", "os.features",
        "variant", "config", "rootfs", "history",
    }:
        fail("Docker archive image config contains an unapproved top-level field")
    if archive_config.get("os") != "linux" or archive_config.get("architecture") != "amd64":
        fail("Docker archive config platform is not linux/amd64")
    if archive_config.get("author") not in (None, "") or archive_config.get("os.version") not in (None, ""):
        fail("Docker archive author/OS-version metadata is not empty")
    if archive_config.get("os.features") not in (None, []) or archive_config.get("variant") not in (None, ""):
        fail("Docker archive platform feature metadata is invalid")
    if not isinstance(archive_config.get("history"), list):
        fail("Docker archive history is invalid")
    rootfs = archive_config.get("rootfs")
    if not isinstance(rootfs, dict) or rootfs.get("type") != "layers":
        fail("Docker archive config rootfs type is invalid")
    diff_ids = rootfs.get("diff_ids")
    if not isinstance(diff_ids, list) or any(
        not isinstance(digest, str) or IMAGE_ID.fullmatch(digest) is None for digest in diff_ids
    ):
        fail("Docker archive config rootfs.diff_ids is invalid")
    if len(diff_ids) != len(layer_diff_ids):
        fail("Docker archive layer count differs from config rootfs.diff_ids")
    if diff_ids != layer_diff_ids:
        fail("Docker archive layer bytes/order differ from config rootfs.diff_ids")
    created = source["SOURCE_CREATED_RFC3339"]
    if archive_config.get("created") != created:
        fail("Docker archive creation time does not equal the locked commit time")
    labels = expected_labels(source, source_lock_sha, toolchain_lock_sha)
    archived_runtime = validate_runtime_config(archive_config.get("config"), labels, "archive")

    inspected = read_json(inspect_path)
    if not isinstance(inspected, list) or len(inspected) != 1 or not isinstance(inspected[0], dict):
        fail("image-inspect.json must contain exactly one image")
    image = inspected[0]
    if image.get("Id") != image_id:
        fail("image inspect ID differs from the external/archive image ID")
    if image.get("Os") != "linux" or image.get("Architecture") != "amd64":
        fail("image inspect platform is not linux/amd64")
    if image.get("Created") != created:
        fail("image inspect creation time does not equal the locked commit time")
    inspected_runtime = validate_runtime_config(image.get("Config"), labels, "inspect")
    if inspected_runtime != archived_runtime:
        fail("image inspect and archive runtime configs disagree")


def verify_build_bundle(
    root: Path,
    repo_root: Path,
    trusted_source_lock: Path,
    trusted_toolchain_lock: Path,
    require_exact_directory: bool,
) -> str:
    if root.is_symlink() or not root.is_dir():
        fail("build bundle must be a non-symlink directory")
    for trusted in (trusted_source_lock, trusted_toolchain_lock):
        if trusted.is_symlink() or not trusted.is_file():
            fail(f"trusted lock is missing or symlinked: {trusted.name}")
    for name in BUILD_ARTIFACTS | {"build.json"}:
        path = root / name
        if path.is_symlink() or not path.is_file():
            fail(f"build artifact is missing or symlinked: {name}")
    if require_exact_directory:
        try:
            entries = {path.name for path in root.iterdir()}
        except OSError as exc:
            fail(f"cannot enumerate build bundle: {exc}")
        if entries != BUILD_ARTIFACTS | {"build.json"}:
            fail("build bundle contains an unapproved entry")

    if (root / "source.lock").read_bytes() != trusted_source_lock.read_bytes():
        fail("build source.lock differs from the trusted checked-in lock")
    if (root / "toolchain.lock").read_bytes() != trusted_toolchain_lock.read_bytes():
        fail("build toolchain.lock differs from the trusted checked-in lock")
    source = verify_trusted_source(repo_root, trusted_source_lock)
    toolchain = read_canonical_lock(trusted_toolchain_lock, TOOLCHAIN_KEYS)
    if toolchain["TARGET_PLATFORM"] != "linux/amd64":
        fail("trusted toolchain target is not linux/amd64")

    manifest = read_json(root / "build.json")
    expected_keys = {
        "format",
        "image_id",
        "local_retention_reference",
        "target_platform",
        "source_revision",
        "source_created_rfc3339",
        "source_tree_sha256",
        "source_lock_sha256",
        "toolchain_lock_sha256",
        "reproducible_image_id",
        "artifacts",
    }
    if not isinstance(manifest, dict) or set(manifest) != expected_keys or manifest.get("format") != 2:
        fail("build.json schema changed")
    image_id = manifest.get("image_id")
    if not isinstance(image_id, str) or IMAGE_ID.fullmatch(image_id) is None:
        fail("build.json image ID is invalid")
    local_retention_reference = manifest.get("local_retention_reference")
    if (
        not isinstance(local_retention_reference, str)
        or LOCAL_RETENTION_REF.fullmatch(local_retention_reference) is None
    ):
        fail("build.json local retention reference is invalid")
    source_lock_sha = sha256(trusted_source_lock)
    toolchain_lock_sha = sha256(trusted_toolchain_lock)
    expected_values = {
        "target_platform": toolchain["TARGET_PLATFORM"],
        "source_revision": source["SOURCE_REVISION"],
        "source_created_rfc3339": source["SOURCE_CREATED_RFC3339"],
        "source_tree_sha256": source["SOURCE_TREE_SHA256"],
        "source_lock_sha256": source_lock_sha,
        "toolchain_lock_sha256": toolchain_lock_sha,
        "reproducible_image_id": True,
    }
    for key, expected in expected_values.items():
        if manifest.get(key) != expected:
            fail(f"build.json does not match trusted build input: {key}")
    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, dict) or set(artifacts) != BUILD_ARTIFACTS:
        fail("build.json artifact allowlist changed")
    for name, expected in artifacts.items():
        if not isinstance(expected, str) or HEX64.fullmatch(expected) is None:
            fail(f"build.json artifact digest is invalid: {name}")
        if sha256(root / name) != expected:
            fail(f"build artifact hash mismatch: {name}")

    try:
        image_reference = (root / "edge-image-reference.txt").read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read edge image reference: {exc}")
    if image_reference != image_id + "\n":
        fail("external edge image reference differs from build.json")
    try:
        retention_reference = (root / "local-image-retention-reference.txt").read_text(
            encoding="ascii"
        )
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read local image retention reference: {exc}")
    if retention_reference != local_retention_reference + "\n":
        fail("local image retention reference differs from build.json")
    expected_tar_line = sha256(root / "image.tar") + "  image.tar\n"
    try:
        actual_tar_line = (root / "image.tar.sha256").read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read image.tar checksum: {exc}")
    if actual_tar_line != expected_tar_line:
        fail("image.tar checksum is not canonical or does not match")
    verify_archive(
        root / "image.tar",
        root / "image-inspect.json",
        image_id,
        source,
        source_lock_sha,
        toolchain_lock_sha,
    )
    return image_id


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bundle", required=True, type=Path)
    parser.add_argument("--repo-root", required=True, type=Path)
    parser.add_argument("--source-lock", required=True, type=Path)
    parser.add_argument("--toolchain-lock", required=True, type=Path)
    parser.add_argument("--allow-extra", action="store_true")
    args = parser.parse_args()
    image_id = verify_build_bundle(
        args.bundle,
        args.repo_root,
        args.source_lock,
        args.toolchain_lock,
        not args.allow_extra,
    )
    print(f"OK: build bundle binds archive, inspect, locks, and image ID {image_id}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
