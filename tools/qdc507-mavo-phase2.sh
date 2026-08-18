#!/bin/bash

set -euo pipefail
umask 077

readonly ROOT="$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd)"
readonly PHASE1_CHECK="$ROOT/tools/qdc507-mavo-phase1.sh"
readonly CONFIRMATION='一次 MaVo Phase-2 临时运行时探测，结束后卸载清理并恢复，不拨号不发短信'

mode="check"
confirmation=""

usage() {
  /bin/cat <<'EOF'
Usage:
  tools/qdc507-mavo-phase2.sh [check]
  tools/qdc507-mavo-phase2.sh apply --confirm '一次 MaVo Phase-2 临时运行时探测，结束后卸载清理并恢复，不拨号不发短信'

check is the default. It verifies the fixed local three-file runtime cache and
delegates only to the frozen Phase-1 read-only factory/USB/modem preflight.

apply has a separate exact confirmation, but the live mutation adapter is
intentionally not wired. It currently repeats check and exits LOCKED without
changing the USB profile, pushing a file, loading a module, starting audio,
placing a call, or changing SMS state.
EOF
}

if [[ $# -gt 0 && "$1" != --* ]]; then
  mode="$1"
  shift
fi
while [[ $# -gt 0 ]]; do
  case "$1" in
    --confirm)
      [[ $# -ge 2 ]] || { printf '%s\n' 'ERROR: --confirm requires the exact Phase-2 phrase' >&2; exit 2; }
      confirmation="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'ERROR: unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ "$mode" == "check" || "$mode" == "apply" ]] || {
  printf 'ERROR: mode must be check or apply, not %s\n' "$mode" >&2
  exit 2
}
if [[ "$mode" == "apply" && "$confirmation" != "$CONFIRMATION" ]]; then
  printf '%s\n' 'ERROR: apply requires the exact independent Phase-2 confirmation phrase' >&2
  exit 2
fi
if [[ "$mode" == "check" && -n "$confirmation" ]]; then
  printf '%s\n' 'ERROR: --confirm is accepted only with apply' >&2
  exit 2
fi

testing="${DJONEHUB_PHASE2_TESTING:-0}"
if [[ "$testing" == "1" ]]; then
  runtime_directory="${DJONEHUB_PHASE2_TEST_CACHE:-}"
  [[ -n "$runtime_directory" && "$runtime_directory" == /* ]] || {
    printf '%s\n' 'ERROR: hermetic Phase-2 test cache must be absolute' >&2
    exit 2
  }
else
  [[ -z "${DJONEHUB_PHASE2_TEST_CACHE:-}" ]] || {
    printf '%s\n' 'ERROR: test cache override is unavailable outside hermetic mode' >&2
    exit 2
  }
  runtime_directory="$HOME/Library/Application Support/MacCellular Runtime/ModuleVoice"
fi

/usr/bin/python3 - "$runtime_directory" "$testing" <<'PY'
import hashlib
import os
import stat
import sys

directory, testing = sys.argv[1:3]

production = (
    ("mavo-pcm-bridge.armv7", "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc"),
    ("qdc507_aprv3.ko", "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a"),
    ("qdc507_voice.ko", "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c"),
)
hermetic = (
    ("mavo-pcm-bridge.armv7", "01747e0173ab80aed9dbfe823e76c3706949c8ec3286164debc8aa57b556e5d1"),
    ("qdc507_aprv3.ko", "c36ff78883d5574fc4455e57c8f71f7d87f785d1700cc6475d544d61ea9b89f6"),
    ("qdc507_voice.ko", "114a323bd4a3d9394a5b13001b474dde421952c9b75c76b7ca8a6799cf0406ed"),
)
policy = hermetic if testing == "1" else production


def fail(message):
    print(f"phase2_cache=FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


try:
    directory_stat = os.lstat(directory)
except OSError as exc:
    fail(f"local cache unavailable: {type(exc).__name__}")
if not stat.S_ISDIR(directory_stat.st_mode) or stat.S_ISLNK(directory_stat.st_mode):
    fail("local cache is not a real directory")
if stat.S_IMODE(directory_stat.st_mode) & 0o022:
    fail("local cache is group/world writable")

verified = []
for name, expected in policy:
    path = os.path.join(directory, name)
    try:
        before = os.lstat(path)
    except OSError as exc:
        fail(f"{name} unavailable: {type(exc).__name__}")
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        fail(f"{name} is not a regular file")
    if before.st_nlink != 1 or stat.S_IMODE(before.st_mode) & 0o022:
        fail(f"{name} has unsafe links or permissions")
    if before.st_size <= 0 or before.st_size > 16 * 1024 * 1024:
        fail(f"{name} has an invalid size")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        try:
            opened = os.fstat(descriptor)
            if (opened.st_dev, opened.st_ino, opened.st_size) != (before.st_dev, before.st_ino, before.st_size):
                fail(f"{name} changed while being verified")
            digest = hashlib.sha256()
            total = 0
            while True:
                chunk = os.read(descriptor, 65536)
                if not chunk:
                    break
                total += len(chunk)
                if total > 16 * 1024 * 1024:
                    fail(f"{name} exceeded the fixed size bound")
                digest.update(chunk)
        finally:
            os.close(descriptor)
    except OSError as exc:
        fail(f"{name} could not be verified: {type(exc).__name__}")
    if total != before.st_size or digest.hexdigest() != expected:
        fail(f"{name} failed fixed SHA-256 verification")
    verified.append((name, expected, total))

print("phase2_cache=PASS")
print("runtime_files=3")
for name, digest, size in verified:
    print(f"runtime_artifact={name} sha256={digest} size={size}")
print("runtime_source=local_fixed_cache")
PY

"$PHASE1_CHECK" check
printf '%s\n' 'phase2_preflight=PASS'
printf '%s\n' 'mutation_sent=no'
printf '%s\n' 'call_command_sent=no'
printf '%s\n' 'sms_mutation_sent=no'

if [[ "$mode" == "apply" ]]; then
  printf '%s\n' 'phase2_apply=LOCKED_NOT_AUTHORIZED' >&2
  printf '%s\n' 'live_adapter_wired=no' >&2
  exit 23
fi

