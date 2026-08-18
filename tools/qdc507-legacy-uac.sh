#!/bin/bash

set -euo pipefail
umask 077

readonly CONFIRMATION='一次 legacy UAC profile 往返，自动恢复，不推模块、不拨号'
mode="check"
confirmation=""
base_url="${DJONEHUB_LEGACY_UAC_BASE_URL:-http://127.0.0.1:7576}"

usage() {
  /bin/cat <<'EOF'
Usage:
  tools/qdc507-legacy-uac.sh [check]
  tools/qdc507-legacy-uac.sh apply --confirm '一次 legacy UAC profile 往返，自动恢复，不推模块、不拨号'

check is read-only. apply is a separate, explicit legacy UAC-only round-trip:
it changes only the USB composition, performs no module-runtime push/load and
issues no call command; the backend restores the factory composition on every
failure or after the UAC/CoreAudio/QPCMV probe.
EOF
}

if [[ $# -gt 0 && "$1" != --* ]]; then
  mode="$1"
  shift
fi
while [[ $# -gt 0 ]]; do
  case "$1" in
    --confirm)
      [[ $# -ge 2 ]] || { printf '%s\n' 'ERROR: --confirm requires the exact confirmation phrase' >&2; exit 2; }
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
[[ "$base_url" =~ ^http://127\.0\.0\.1:[0-9]{1,5}$ ]] || {
  printf '%s\n' 'ERROR: legacy UAC API must be an explicit 127.0.0.1 HTTP endpoint' >&2
  exit 2
}
if [[ "$mode" == "apply" && "$confirmation" != "$CONFIRMATION" ]]; then
  printf '%s\n' 'ERROR: apply requires the exact one-time legacy UAC confirmation phrase' >&2
  exit 2
fi
if [[ "$mode" == "check" && -n "$confirmation" ]]; then
  printf '%s\n' 'ERROR: --confirm is accepted only with apply' >&2
  exit 2
fi

exec /usr/bin/python3 - "$mode" "$base_url" "$confirmation" <<'PY'
import json
import re
import sys
import time
import urllib.error
import urllib.request

MODE, BASE_URL, CONFIRMATION = sys.argv[1:4]
EXPECTED = (0x2CA3, 0x4006, (1, 1, 1, 1, 1, 0, 0))
LEGACY_TARGET = (0x2C7C, 0x0125, (1, 1, 1, 1, 1, 0, 1))


class GateError(RuntimeError):
    pass


def request_json(method, path, payload=None, timeout=25):
    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(BASE_URL + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as exc:
        raise GateError(f"local API rejected {path} with HTTP {exc.code}") from None
    except (OSError, ValueError) as exc:
        raise GateError(f"local API unavailable or invalid at {path}: {type(exc).__name__}") from None
    if not isinstance(body, dict):
        raise GateError(f"local API returned a non-object at {path}")
    return body


def at_query(command, allow_error=False):
    body = request_json("POST", "/api/at", {"command": command})
    response = body.get("response")
    if not isinstance(response, str) or len(response) > 8192:
        raise GateError("diagnostic AT response was missing or oversized")
    if allow_error and re.search(r"(?:^|\n)ERROR\s*(?:\n|$)", response, re.I):
        return response
    lines = [line.strip() for line in response.replace("\r", "").split("\n") if line.strip()]
    if not lines or lines[-1].upper() != "OK" or sum(line.upper() == "OK" for line in lines) != 1:
        raise GateError("diagnostic AT response did not have one terminal OK")
    if any(line.upper() == "ERROR" or line.upper().startswith(("+CME ERROR", "+CMS ERROR")) for line in lines):
        raise GateError("diagnostic AT response was rejected")
    return lines


def parse_factory(lines):
    matches = []
    for line in lines:
        match = re.fullmatch(
            r'\+QCFG:\s*"usbcfg",\s*(0x[0-9a-f]+|\d+),\s*(0x[0-9a-f]+|\d+),'
            r'\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01])',
            line, re.I,
        )
        if match:
            matches.append((int(match.group(1), 0), int(match.group(2), 0), tuple(int(match.group(i)) for i in range(3, 10))))
    if len(matches) != 1 or matches[0] != EXPECTED:
        raise GateError("module is not in the exact factory USB profile")
    return matches[0]


def parse_usb_profile(lines):
    matches = []
    for line in lines:
        match = re.fullmatch(
            r'\+QCFG:\s*"usbcfg",\s*(0x[0-9a-f]+|\d+),\s*(0x[0-9a-f]+|\d+),'
            r'\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01])',
            line, re.I,
        )
        if match:
            matches.append((int(match.group(1), 0), int(match.group(2), 0), tuple(int(match.group(i)) for i in range(3, 10))))
    if len(matches) != 1:
        raise GateError("module did not return one exact USB profile during recovery")
    return matches[0]


def preflight():
    state = str(request_json("GET", "/api/module/setup").get("state", ""))
    if state in {"initializing", "restarting", "verifying", "legacy_uac_running", "mavo_phase1_running"}:
        raise GateError("another module transaction is active")
    parse_factory(at_query('AT+QCFG="USBCFG"'))
    ims = at_query('AT+QCFG="ims"')
    qpcmv = at_query("AT+QPCMV?", allow_error=True)
    print("legacy_uac_preflight=PASS")
    print("factory_profile=2ca3:4006:1,1,1,1,1,0,0")
    print("ims_query=available")
    print("qpcmv_query=" + ("error" if "ERROR" in qpcmv.upper() else "available"))
    return ims


def apply_roundtrip():
    preflight()
    started = request_json("POST", "/api/module/setup", {
        "confirm": True,
        "experiment": "legacy_uac_roundtrip",
        "legacy_uac_confirmation": CONFIRMATION,
    })
    if started.get("state") != "legacy_uac_running":
        raise GateError("backend did not enter the legacy UAC transaction")
    # The module can take a little over three minutes to re-enumerate on this
    # Mac. Leave enough room for target inspection and a separately bounded
    # factory rollback instead of timing out while the backend is still safe.
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        status = request_json("GET", "/api/module/setup")
        state = str(status.get("state", ""))
        if state == "legacy_uac_complete":
            print("legacy_uac_roundtrip=PASS")
            print("factory_restore=verified")
            return 0
        if state == "legacy_uac_rolled_back":
            print("legacy_uac_roundtrip=FAILED_SAFE", file=sys.stderr)
            print("factory_restore=verified", file=sys.stderr)
            return 20
        if state == "legacy_uac_recovery_required":
            print("legacy_uac_roundtrip=RECOVERY_REQUIRED", file=sys.stderr)
            print("factory_restore=retrying", file=sys.stderr)
            return recover_factory()
        if state == "legacy_uac_failed":
            print("legacy_uac_roundtrip=STOPPED_BEFORE_WRITE", file=sys.stderr)
            return 22
        if state != "legacy_uac_running":
            raise GateError("backend returned an unexpected legacy UAC state")
        time.sleep(2)
    raise GateError("legacy UAC status timed out; do not unplug the module")


def recover_factory():
    """Use the same one-time authorization only to finish mandatory rollback."""
    # A bounded transaction can report recovery_required just before the slow
    # module becomes reachable again. Wait read-only for either the exact
    # target or an already-restored factory profile before asking the backend
    # to perform the fixed rollback.
    profile = None
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        try:
            profile = parse_usb_profile(at_query('AT+QCFG="USBCFG"'))
        except GateError:
            time.sleep(2)
            continue
        if profile in {EXPECTED, LEGACY_TARGET}:
            break
        raise GateError("unexpected USB profile appeared during legacy UAC recovery")
    if profile == EXPECTED:
        print("legacy_uac_roundtrip=FAILED_SAFE", file=sys.stderr)
        print("factory_restore=verified", file=sys.stderr)
        return 20
    if profile != LEGACY_TARGET:
        print("factory_restore=unverified", file=sys.stderr)
        return 21

    started = request_json("POST", "/api/module/setup", {
        "confirm": True,
        "experiment": "legacy_uac_restore",
        "legacy_uac_confirmation": CONFIRMATION,
    })
    if started.get("state") != "legacy_uac_recovery_running":
        raise GateError("backend did not enter the bounded legacy UAC recovery")
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        status = request_json("GET", "/api/module/setup")
        state = str(status.get("state", ""))
        if state == "legacy_uac_rolled_back":
            print("legacy_uac_roundtrip=FAILED_SAFE", file=sys.stderr)
            print("factory_restore=verified", file=sys.stderr)
            return 20
        if state == "legacy_uac_recovery_required":
            print("factory_restore=unverified", file=sys.stderr)
            return 21
        if state != "legacy_uac_recovery_running":
            raise GateError("backend returned an unexpected legacy UAC recovery state")
        time.sleep(2)
    raise GateError("legacy UAC recovery timed out; do not unplug the module")


try:
    preflight()
    if MODE == "apply":
        raise SystemExit(apply_roundtrip())
except GateError as exc:
    print(f"legacy_uac_preflight=FAIL: {exc}", file=sys.stderr)
    raise SystemExit(1)
PY
