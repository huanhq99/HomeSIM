#!/usr/bin/env python3
"""Create source.lock from one committed, clean selected source revision."""

from __future__ import annotations

import argparse
import datetime as dt
import importlib.util
import os
import sys
import tempfile
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("verify-source.py")
sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("verify_source", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise SystemExit("ERROR: cannot load verify-source.py")
verify_source = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(verify_source)


def git(root: Path, *args: str) -> str:
    return verify_source.git(root, *args).strip()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo-root", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    root = args.repo_root.resolve()
    revision = args.revision
    top = Path(git(root, "rev-parse", "--show-toplevel")).resolve()
    if top != root:
        verify_source.fail("--repo-root is not the physical Git worktree root")
    verify_source.reject_repository_rewrites(root)
    if len(revision) != 40 or any(char not in "0123456789abcdef" for char in revision):
        verify_source.fail("revision must be a full lowercase Git commit")
    if git(root, "rev-parse", "HEAD") != revision:
        verify_source.fail("revision must equal the checked-out HEAD")
    files = verify_source.selected_files(root)
    relative = [path.relative_to(root).as_posix() for path in files]
    tracked = set(git(root, "ls-tree", "-r", "--name-only", revision).splitlines())
    missing = [path for path in relative if path not in tracked]
    if missing:
        verify_source.fail(f"selected build input is not committed at revision: {missing[0]}")
    dirty = git(root, "status", "--porcelain", "--", *verify_source.SELECTED)
    if dirty:
        verify_source.fail("selected build inputs are dirty; commit them before refreshing source.lock")
    epoch_text = git(root, "show", "-s", "--format=%ct", revision)
    if not epoch_text.isdigit() or int(epoch_text) < 1:
        verify_source.fail("commit epoch is invalid")
    epoch = int(epoch_text)
    created = dt.datetime.fromtimestamp(epoch, tz=dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    digest = verify_source.tree_digest(root, files)
    content = (
        "FORMAT=1\n"
        f"SOURCE_REVISION={revision}\n"
        f"SOURCE_DATE_EPOCH={epoch}\n"
        f"SOURCE_CREATED_RFC3339={created}\n"
        f"SOURCE_FILE_COUNT={len(files)}\n"
        f"SOURCE_TREE_SHA256={digest}\n"
    )
    output = args.output
    if output.parent.resolve() != output.parent or output.parent.is_symlink() or not output.parent.is_dir():
        verify_source.fail("source.lock parent must be a real directory")
    fd, temporary = tempfile.mkstemp(prefix=".source.lock.", dir=output.parent)
    try:
        with os.fdopen(fd, "w", encoding="ascii", newline="\n") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    verified = verify_source.load_lock(output)
    verify_source.verify_repository_binding(root, verified, files)
    if verify_source.tree_digest(root, files) != verified["SOURCE_TREE_SHA256"]:
        verify_source.fail("new source.lock failed its exact source-tree verification")
    print(f"OK: source.lock binds committed revision {revision} and {len(files)} files")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
