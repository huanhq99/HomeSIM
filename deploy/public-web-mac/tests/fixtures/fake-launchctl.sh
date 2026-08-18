#!/bin/bash

set -euo pipefail

state_dir="${FAKE_LAUNCHCTL_STATE_DIR:?FAKE_LAUNCHCTL_STATE_DIR is required}"
/usr/bin/install -d -m 700 "$state_dir"

command_name="${1:-}"
case "$command_name" in
  print)
    target="${2:?print target is required}"
    label="${target##*/}"
    [[ -f "$state_dir/$label" ]] || exit 113
    printf 'state = running\n'
    printf 'pid = 4242\n'
    ;;
  bootout)
    target="${2:?bootout target is required}"
    label="${target##*/}"
    /bin/rm -f -- "$state_dir/$label"
    if [[ "$label" == "io.maccellular.phone" && -n "${FAKE_REMOTE_MEDIA_SOCKET:-}" ]]; then
      /bin/rm -f -- "$FAKE_REMOTE_MEDIA_SOCKET"
    fi
    printf 'bootout %s\n' "$label" >> "$state_dir/operations.log"
    ;;
  bootstrap)
    domain="${2:?bootstrap domain is required}"
    plist="${3:?bootstrap plist is required}"
    [[ "$domain" == gui/* ]]
    [[ -f "$plist" && ! -L "$plist" ]]
    label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$plist")"
    if /usr/bin/grep -Fq '/Documents/' "$plist"; then
      printf 'fixture rejected a Documents-dependent LaunchAgent: %s\n' "$plist" >&2
      exit 78
    fi
    if [[ "${FAKE_LAUNCHCTL_FAIL_ONCE_LABEL:-}" == "$label" && ! -e "$state_dir/.failed-once-$label" ]]; then
      /usr/bin/touch "$state_dir/.failed-once-$label"
      printf 'fail-once %s\n' "$label" >> "$state_dir/operations.log"
      exit 79
    fi
    printf '%s\n' "$plist" > "$state_dir/$label"
    if [[ "$label" == "io.maccellular.phone" && -n "${FAKE_REMOTE_MEDIA_SOCKET:-}" ]] && \
       /usr/bin/grep -Fq -- '-public-web-direct-voice' "$plist"; then
      /usr/bin/python3 - "$FAKE_REMOTE_MEDIA_SOCKET" <<'PY'
import os
import pathlib
import socket
import sys

path = pathlib.Path(sys.argv[1])
path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
os.chmod(path.parent, 0o700)
try:
    path.unlink()
except FileNotFoundError:
    pass
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
try:
    sock.bind(str(path))
    os.chmod(path, 0o600)
finally:
    sock.close()
PY
    fi
    printf 'bootstrap %s\n' "$label" >> "$state_dir/operations.log"
    ;;
  *)
    printf 'unsupported fake launchctl command: %s\n' "$command_name" >&2
    exit 64
    ;;
esac
