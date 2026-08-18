#!/usr/bin/env python3
"""Verify and optionally stage the exact public-edge build context."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import os
import stat
import subprocess
import sys
from pathlib import Path


SELECTED = (
    "go.mod",
    "go.sum",
    "cmd/djonehub-edge",
    "internal/publicedge",
    "internal/publicedgeserver",
    "third_party",
    # source.lock is the sole intentional exclusion from this workflow closure:
    # including the generated lock would make its own digest recursive.  The
    # lock, toolchain policy, and every script that can build/audit/publish or
    # interpret provenance must otherwise come from the same committed tree.
    "deploy/public-edge/image/Dockerfile",
    "deploy/public-edge/image/atomic-commit.py",
    "deploy/public-edge/image/audit-image.sh",
    "deploy/public-edge/image/build-image.sh",
    "deploy/public-edge/image/docker-cli-root-config.json",
    "deploy/public-edge/image/healthcheck.go",
    "deploy/public-edge/image/inspect-rootfs.py",
    "deploy/public-edge/image/publish-image.sh",
    "deploy/public-edge/image/refresh-source-lock.py",
    "deploy/public-edge/image/sanitize-image-archive.py",
    "deploy/public-edge/image/toolchain.lock",
    "deploy/public-edge/image/verify-build-bundle.py",
    "deploy/public-edge/image/verify-evidence.py",
    "deploy/public-edge/image/verify-source.py",
    "deploy/public-edge/compose.yaml",
    "deploy/public-edge/deploy.sh",
    "deploy/public-edge/generate-config.sh",
    "deploy/public-edge/templates",
    "deploy/public-edge/verify-compose-policy.py",
    "deploy/public-edge/verify-action-evidence.py",
    "deploy/public-edge/verify-config.sh",
    "deploy/public-edge/verify-runtime-state.py",
)
LOCK_KEYS = (
    "FORMAT",
    "SOURCE_REVISION",
    "SOURCE_DATE_EPOCH",
    "SOURCE_CREATED_RFC3339",
    "SOURCE_FILE_COUNT",
    "SOURCE_TREE_SHA256",
)


def fail(message: str) -> "None":
    raise SystemExit(f"ERROR: {message}")


def git_environment() -> dict[str, str]:
    """Return a minimal environment for every provenance-sensitive Git call."""
    return {
        "PATH": os.environ.get("PATH", os.defpath),
        "HOME": "/nonexistent-dji4g-public-edge-git-home",
        "LANG": "C",
        "LC_ALL": "C",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_OPTIONAL_LOCKS": "0",
        "GIT_PAGER": "cat",
        "GIT_TERMINAL_PROMPT": "0",
    }


def git_command(*args: str) -> list[str]:
    return [
        "git",
        "--no-replace-objects",
        "-c",
        "core.fsmonitor=false",
        "-c",
        "core.untrackedCache=false",
        *args,
    ]


def git(root: Path, *args: str, text: bool = True):
    try:
        return subprocess.check_output(
            git_command(*args), cwd=root, env=git_environment(), text=text,
            encoding="utf-8" if text else None,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        fail(f"Git verification failed: {' '.join(args)}: {exc}")


def git_success(root: Path, *args: str) -> bool:
    try:
        subprocess.run(
            git_command(*args), cwd=root, env=git_environment(),
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True,
        )
    except (OSError, subprocess.CalledProcessError):
        return False
    return True


def repository_git_path(root: Path, value: str, label: str) -> Path:
    candidate = Path(value)
    if not candidate.is_absolute():
        candidate = root / candidate
    try:
        physical = candidate.resolve(strict=True)
    except OSError as exc:
        fail(f"cannot resolve repository {label}: {exc}")
    if not physical.is_dir():
        fail(f"repository {label} is not a directory")
    return physical


def reject_repository_rewrites(root: Path) -> None:
    """Reject persistent object rewriting or external object-store inputs."""
    git_dir = repository_git_path(
        root, git(root, "rev-parse", "--absolute-git-dir").strip(), "Git directory"
    )
    common_dir = repository_git_path(
        root, git(root, "rev-parse", "--git-common-dir").strip(), "common Git directory"
    )
    replacements = git(
        root, "for-each-ref", "--format=%(refname)", "refs/replace"
    ).splitlines()
    if replacements:
        fail("repository replace refs are forbidden for source provenance")
    for directory in {git_dir, common_dir}:
        for relative, label in (
            ("info/grafts", "legacy grafts"),
            ("objects/info/alternates", "object alternates"),
            ("objects/info/http-alternates", "HTTP object alternates"),
        ):
            path = directory / relative
            if path.exists() or path.is_symlink():
                fail(f"repository {label} are forbidden for source provenance")


def load_lock(path: Path) -> dict[str, str]:
    try:
        text = path.read_text(encoding="ascii")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read source lock: {exc}")
    if not text.endswith("\n") or "\r" in text or "\x00" in text:
        fail("source lock is not canonical ASCII lines")
    result: dict[str, str] = {}
    for line in text[:-1].split("\n"):
        if line.count("=") != 1:
            fail("source lock contains a malformed line")
        key, value = line.split("=", 1)
        if key in result:
            fail(f"duplicate source lock key: {key}")
        result[key] = value
    if tuple(result) != LOCK_KEYS or result["FORMAT"] != "1":
        fail("source lock schema or key order changed")
    if len(result["SOURCE_REVISION"]) != 40 or any(
        char not in "0123456789abcdef" for char in result["SOURCE_REVISION"]
    ):
        fail("source revision is invalid")
    if not result["SOURCE_DATE_EPOCH"].isdigit() or int(result["SOURCE_DATE_EPOCH"]) < 1:
        fail("source epoch is invalid")
    if not result["SOURCE_CREATED_RFC3339"].endswith("Z") or len(result["SOURCE_CREATED_RFC3339"]) != 20:
        fail("source creation time is invalid")
    if not result["SOURCE_FILE_COUNT"].isdigit() or int(result["SOURCE_FILE_COUNT"]) < 1:
        fail("source file count is invalid")
    digest = result["SOURCE_TREE_SHA256"]
    if len(digest) != 64 or any(char not in "0123456789abcdef" for char in digest):
        fail("source tree digest is invalid")
    return result


def selected_files(root: Path) -> list[Path]:
    files: list[Path] = []
    for relative in SELECTED:
        path = root / relative
        try:
            mode = path.lstat().st_mode
        except OSError as exc:
            fail(f"required build input is missing: {relative}: {exc}")
        if stat.S_ISLNK(mode):
            fail(f"build input may not be a symlink: {relative}")
        if stat.S_ISREG(mode):
            files.append(path)
            continue
        if not stat.S_ISDIR(mode):
            fail(f"build input has unsupported type: {relative}")
        for candidate in path.rglob("*"):
            candidate_mode = candidate.lstat().st_mode
            candidate_relative = candidate.relative_to(root).as_posix()
            if stat.S_ISLNK(candidate_mode):
                fail(f"build input may not be a symlink: {candidate_relative}")
            if stat.S_ISREG(candidate_mode):
                files.append(candidate)
            elif not stat.S_ISDIR(candidate_mode):
                fail(f"build input has unsupported type: {candidate_relative}")
    unique = {path.relative_to(root).as_posix(): path for path in files}
    if len(unique) != len(files):
        fail("build input selection contains a duplicate path")
    return [unique[name] for name in sorted(unique)]


def tree_digest(root: Path, files: list[Path]) -> str:
    aggregate = hashlib.sha256()
    for path in files:
        relative = path.relative_to(root).as_posix()
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        aggregate.update(f"{digest}  {relative}\n".encode("ascii"))
    return aggregate.hexdigest()


def verify_repository_binding(
    root: Path, lock: dict[str, str], files: list[Path]
) -> dict[str, str]:
    """Prove that the lock describes a reachable commit and its selected blobs."""

    top = Path(git(root, "rev-parse", "--show-toplevel").strip()).resolve()
    if top != root:
        fail("--repo-root is not the physical Git worktree root")
    reject_repository_rewrites(root)

    revision = lock["SOURCE_REVISION"]
    resolved = git(root, "rev-parse", "--verify", f"{revision}^{{commit}}").strip()
    if resolved != revision:
        fail("source revision does not resolve to the exact locked commit")
    if not git_success(root, "merge-base", "--is-ancestor", revision, "HEAD"):
        fail("source revision is not an ancestor of the checked-out HEAD")

    epoch_text = git(root, "show", "-s", "--format=%ct", revision).strip()
    if not epoch_text.isdigit() or int(epoch_text) < 1:
        fail("locked commit has an invalid committer epoch")
    expected_created = dt.datetime.fromtimestamp(
        int(epoch_text), tz=dt.timezone.utc
    ).strftime("%Y-%m-%dT%H:%M:%SZ")
    if lock["SOURCE_DATE_EPOCH"] != epoch_text:
        fail("source epoch does not equal the locked commit committer epoch")
    if lock["SOURCE_CREATED_RFC3339"] != expected_created:
        fail("source creation time was not derived exactly from the locked commit epoch")

    raw_tree = git(
        root, "ls-tree", "-r", "--full-tree", "-z", revision, "--", *SELECTED,
        text=False,
    )
    commit_blobs: dict[str, str] = {}
    for record in raw_tree.split(b"\0"):
        if not record:
            continue
        try:
            metadata, raw_path = record.split(b"\t", 1)
            mode, object_type, object_id = metadata.decode("ascii").split(" ")
            relative = raw_path.decode("utf-8")
        except (ValueError, UnicodeError):
            fail("locked commit contains a malformed selected tree entry")
        if object_type != "blob" or mode == "120000":
            fail(f"locked commit selected input is not a regular blob: {relative}")
        if relative in commit_blobs:
            fail(f"locked commit contains a duplicate selected path: {relative}")
        commit_blobs[relative] = object_id

    current = {path.relative_to(root).as_posix(): path for path in files}
    if set(current) != set(commit_blobs):
        missing = sorted(set(commit_blobs) - set(current))
        extra = sorted(set(current) - set(commit_blobs))
        detail = (missing or extra)[0]
        kind = "missing" if missing else "uncommitted/extra"
        fail(f"selected path set differs from locked commit ({kind}: {detail})")

    object_format = git(root, "rev-parse", "--show-object-format").strip()
    if object_format not in {"sha1", "sha256"}:
        fail(f"unsupported Git object format: {object_format}")
    constructor = hashlib.sha1 if object_format == "sha1" else hashlib.sha256
    for relative in sorted(current):
        try:
            data = current[relative].read_bytes()
        except OSError as exc:
            fail(f"cannot read selected build input {relative}: {exc}")
        digest = constructor(b"blob " + str(len(data)).encode("ascii") + b"\0" + data).hexdigest()
        if digest != commit_blobs[relative]:
            fail(f"selected file content differs from locked commit: {relative}")
    return commit_blobs


def stage(
    root: Path,
    destination: Path,
    commit_blobs: dict[str, str],
    epoch: int,
    expected_count: int,
    expected_digest: str,
) -> None:
    if destination.is_symlink() or not destination.is_dir():
        fail("context destination must be an existing non-symlink directory")
    if any(destination.iterdir()):
        fail("context destination must be empty")
    aggregate = hashlib.sha256()
    for relative_text in sorted(commit_blobs):
        relative = Path(relative_text)
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        # Export the already-verified locked commit blob, never a second read of
        # the mutable worktree path. This closes the verify/copy TOCTOU window.
        data = git(root, "cat-file", "blob", commit_blobs[relative_text], text=False)
        object_format = git(root, "rev-parse", "--show-object-format").strip()
        constructor = hashlib.sha1 if object_format == "sha1" else hashlib.sha256
        object_id = constructor(
            b"blob " + str(len(data)).encode("ascii") + b"\0" + data
        ).hexdigest()
        if object_id != commit_blobs[relative_text]:
            fail(f"Git returned bytes outside the locked blob: {relative_text}")
        target.write_bytes(data)
        aggregate.update(
            f"{hashlib.sha256(data).hexdigest()}  {relative_text}\n".encode("ascii")
        )
        # Source modes are not semantic Go inputs. Canonicalize them so a Git
        # executable-bit drift cannot alter the Docker context outside the
        # content digest recorded by source.lock.
        os.chmod(target, 0o644)
        os.utime(target, (epoch, epoch), follow_symlinks=False)
    if len(commit_blobs) != expected_count or aggregate.hexdigest() != expected_digest:
        fail("staged commit blobs do not match source.lock")
    dockerfile = destination / "deploy/public-edge/image/Dockerfile"
    (destination / "Dockerfile").write_bytes(dockerfile.read_bytes())
    os.chmod(destination / "Dockerfile", 0o644)
    os.utime(destination / "Dockerfile", (epoch, epoch), follow_symlinks=False)
    (destination / "image").mkdir(mode=0o755)
    (destination / "image/healthcheck.go").write_bytes(
        (destination / "deploy/public-edge/image/healthcheck.go").read_bytes()
    )
    os.chmod(destination / "image/healthcheck.go", 0o644)
    os.utime(destination / "image/healthcheck.go", (epoch, epoch), follow_symlinks=False)
    # The workflow/policy closure is provenance input, not runtime build input.
    # Remove it only after every staged selected blob was re-bound above.
    import shutil

    shutil.rmtree(destination / "deploy")
    for directory in sorted((p for p in destination.rglob("*") if p.is_dir()), reverse=True):
        os.chmod(directory, 0o755)
        os.utime(directory, (epoch, epoch), follow_symlinks=False)
    os.utime(destination, (epoch, epoch), follow_symlinks=False)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo-root", type=Path, required=True)
    parser.add_argument("--lock", type=Path, required=True)
    parser.add_argument("--copy-to", type=Path)
    parser.add_argument("--print", action="store_true", dest="print_values")
    args = parser.parse_args()
    root = args.repo_root.resolve()
    lock = load_lock(args.lock)
    files = selected_files(root)
    commit_blobs = verify_repository_binding(root, lock, files)
    digest = tree_digest(root, files)
    if len(files) != int(lock["SOURCE_FILE_COUNT"]):
        fail("source file count does not match source.lock")
    if digest != lock["SOURCE_TREE_SHA256"]:
        fail("source tree does not match source.lock")
    if args.copy_to is not None:
        stage(
            root,
            args.copy_to,
            commit_blobs,
            int(lock["SOURCE_DATE_EPOCH"]),
            int(lock["SOURCE_FILE_COUNT"]),
            lock["SOURCE_TREE_SHA256"],
        )
    if args.print_values:
        print(f"SOURCE_FILE_COUNT={len(files)}")
        print(f"SOURCE_TREE_SHA256={digest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
