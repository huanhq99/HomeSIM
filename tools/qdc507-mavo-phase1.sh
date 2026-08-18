#!/bin/bash

set -euo pipefail
umask 077

readonly CONFIRMATION='一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号'
mode="check"
confirmation=""
base_url="${DJONEHUB_PHASE1_BASE_URL:-http://127.0.0.1:7576}"

usage() {
  /bin/cat <<'EOF'
Usage:
  tools/qdc507-mavo-phase1.sh [check]
  tools/qdc507-mavo-phase1.sh apply --confirm '一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号'

check is the default and performs read-only modem, USB and CoreAudio preflight.
apply first repeats preflight, then asks the local DJOneHub backend to perform
one ADB+UAC profile round-trip with mandatory automatic factory restoration.
It never pushes or loads a module and never dials, answers or hangs up a call.
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
  printf '%s\n' 'ERROR: phase-1 API must be an explicit 127.0.0.1 HTTP endpoint' >&2
  exit 2
}
if [[ "$mode" == "apply" && "$confirmation" != "$CONFIRMATION" ]]; then
  printf '%s\n' 'ERROR: apply requires the exact one-time phase-1 confirmation phrase' >&2
  exit 2
fi
if [[ "$mode" == "check" && -n "$confirmation" ]]; then
  printf '%s\n' 'ERROR: --confirm is accepted only with apply' >&2
  exit 2
fi

exec /usr/bin/python3 - "$mode" "$base_url" "$confirmation" <<'PY'
import hashlib
import json
import os
import plistlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

MODE, BASE_URL, CONFIRMATION = sys.argv[1:4]
TESTING = os.environ.get("DJONEHUB_PHASE1_TESTING") == "1"
FIXTURE = os.environ.get("DJONEHUB_PHASE1_TEST_FIXTURE", "") if TESTING else ""


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


def at_query(command):
    body = request_json("POST", "/api/at", {"command": command})
    response = body.get("response")
    if not isinstance(response, str) or len(response) > 8192:
        raise GateError("diagnostic AT response was missing or oversized")
    lines = [line.strip() for line in response.replace("\r", "").split("\n") if line.strip()]
    if not lines or lines[-1].upper() != "OK" or sum(line.upper() == "OK" for line in lines) != 1:
        raise GateError("diagnostic AT response did not have one terminal OK")
    if any(line.upper() == "ERROR" or line.upper().startswith("+CME ERROR") for line in lines):
        raise GateError("diagnostic AT query was rejected")
    payload = lines[:-1]
    if payload and payload[0].upper() == command.upper():
        payload = payload[1:]
    if any(line.upper() == command.upper() for line in payload):
        raise GateError("diagnostic AT response contained a duplicate or misplaced command echo")
    return payload


def exact_match(lines, pattern, label, flags=re.IGNORECASE):
    matches = [re.fullmatch(pattern, line, flags) for line in lines]
    matches = [match for match in matches if match]
    if len(matches) != 1:
        raise GateError(f"{label} response was not exact")
    return matches[0]


def strict_clcc(lines):
    seen_echo = False
    indexes = set()
    records = []
    pattern = re.compile(r'^\+CLCC:\s*(\d+),([01]),([0-5]),([0-2]),([01])(?:,"[^"]*",\d+)?$')
    for line in lines:
        if line.upper() == "AT+CLCC":
            if seen_echo or records:
                raise GateError("CLCC framing was ambiguous")
            seen_echo = True
            continue
        match = pattern.fullmatch(line)
        if not match:
            raise GateError("CLCC contained an unrecognized row")
        index, direction, state, call_mode, multiparty = (int(value) for value in match.groups()[:5])
        if index < 1 or index in indexes or multiparty != 0:
            raise GateError("CLCC contained an unsafe index or multiparty row")
        indexes.add(index)
        records.append((index, direction, state, call_mode, multiparty))
    if any(record[3] == 0 for record in records):
        raise GateError("a voice call is active")
    return records


def load_host_snapshot():
    if FIXTURE:
        try:
            fixture = json.loads(open(FIXTURE, "rb").read())
            return fixture["usb"], fixture["audio"]
        except (OSError, ValueError, KeyError, TypeError) as exc:
            raise GateError(f"test fixture is invalid: {type(exc).__name__}") from None
    if sys.platform != "darwin":
        raise GateError("phase-1 host preflight requires macOS")
    try:
        raw_usb = subprocess.run(
            ["/usr/sbin/ioreg", "-a", "-r", "-c", "IOUSBHostDevice"],
            check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15,
        ).stdout
        usb = plistlib.loads(raw_usb)
        raw_audio = subprocess.run(
            ["/usr/sbin/system_profiler", "SPAudioDataType", "-json"],
            check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=30,
        ).stdout
        audio = json.loads(raw_audio)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raise GateError(f"host USB/CoreAudio inspection failed: {type(exc).__name__}") from None
    return usb, audio


def host_gates():
    usb, audio = load_host_snapshot()
    matches = [entry for entry in usb if entry.get("idVendor") == 0x2CA3 and entry.get("idProduct") == 0x4006]
    if len(matches) != 1:
        raise GateError("expected exactly one factory-profile Baiwang USB device")
    device = matches[0]
    location = device.get("locationID")
    if not isinstance(location, int) or location <= 0 or device.get("iSerialNumber") not in (0, None):
        raise GateError("factory USB physical identity was not exact")
    audio_rows = []
    for section in audio.get("SPAudioDataType", []):
        audio_rows.extend(section.get("_items", []))
    markers = ("baiwang", "quectel", "qdc507")
    module_audio = [row for row in audio_rows if any(marker in str(row.get("_name", "")).lower() for marker in markers)]
    if module_audio:
        raise GateError("factory profile unexpectedly exposes a module CoreAudio device")
    return location, len(module_audio)


def preflight():
    setup = request_json("GET", "/api/module/setup")
    state = str(setup.get("state", ""))
    if state in {"initializing", "restarting", "verifying"} or state.startswith("mavo_phase1_running"):
        raise GateError("another module transaction is active")

    usb_lines = at_query('AT+QCFG="USBCFG"')
    usb_match = exact_match(
        usb_lines,
        r'\+QCFG:\s*"usbcfg",\s*(0x[0-9a-f]+|\d+),\s*(0x[0-9a-f]+|\d+),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01]),\s*([01])',
        "USBCFG",
    )
    vendor = int(usb_match.group(1), 0)
    product = int(usb_match.group(2), 0)
    flags = tuple(int(usb_match.group(index)) for index in range(3, 10))
    if (vendor, product, flags) != (0x2CA3, 0x4006, (1, 1, 1, 1, 1, 0, 0)):
        raise GateError("module is not in the exact factory USB profile")

    usbnet = int(exact_match(at_query('AT+QCFG="usbnet"'), r'\+QCFG:\s*"usbnet",\s*([0-3])', "USBNET").group(1))
    ims_match = exact_match(at_query('AT+QCFG="ims"'), r'\+QCFG:\s*"ims",\s*(\d+),\s*(\d+)', "IMS")
    ims, volte_capability = int(ims_match.group(1)), int(ims_match.group(2))
    if volte_capability != 1:
        raise GateError("module does not report VoLTE capability")
    volte_disabled = int(exact_match(at_query('AT+QCFG="volte_disable"'), r'\+QCFG:\s*"(?:volte_disable|volte/disable)",\s*([01])', "VoLTE").group(1))
    if volte_disabled != 0:
        raise GateError("VoLTE is disabled")
    exact_match(at_query("AT+CPIN?"), r'\+CPIN:\s*READY', "SIM")
    cereg = exact_match(at_query("AT+CEREG?"), r'\+CEREG:\s*(?:\d+,)?([015])(?:,.*)?', "CEREG")
    if int(cereg.group(1)) not in (1, 5):
        raise GateError("EPS registration is not ready")
    exact_match(at_query("AT+CGATT?"), r'\+CGATT:\s*1', "packet attachment")
    exact_match(at_query("AT+CFUN?"), r'\+CFUN:\s*1', "CFUN state")
    firmware_lines = at_query("AT+CGMR")
    if firmware_lines != ["QDC507GLEFM21"]:
        raise GateError("firmware is not the fixed QDC507GLEFM21 build")
    identity_lines = at_query("AT+CGSN")
    identity = exact_match(identity_lines, r'(?:\+CGSN:\s*)?(\d{15})', "module identity").group(1)
    identity_digest = hashlib.sha256(identity.encode("ascii")).hexdigest()
    calls = strict_clcc(at_query("AT+CLCC"))
    location, audio_matches = host_gates()

    print("phase1_preflight=PASS")
    print("mutation_sent=no")
    print(f"usb_profile=factory:{vendor:04x}:{product:04x}:" + ",".join(str(value) for value in flags))
    print(f"usb_location=0x{location:08x}")
    print(f"firmware={firmware_lines[0]}")
    print(f"identity_sha256={identity_digest}")
    print(f"usbnet={usbnet} ims={ims} volte_capability={volte_capability} volte_disable={volte_disabled}")
    print(f"clcc_rows={len(calls)} voice_rows=0")
    print(f"coreaudio_module_matches={audio_matches}")


def apply_roundtrip():
    started = request_json("POST", "/api/module/setup", {
        "confirm": True,
        "experiment": "mavo_adb_uac_roundtrip",
        "phase1_confirmation": CONFIRMATION,
    })
    if started.get("state") != "mavo_phase1_running":
        raise GateError("backend did not enter the phase-1 transaction")
    deadline = time.monotonic() + (10 if TESTING else 300)
    while time.monotonic() < deadline:
        status = request_json("GET", "/api/module/setup")
        state = str(status.get("state", ""))
        if state == "mavo_phase1_complete":
            evidence = status.get("backup_path")
            if not isinstance(evidence, str) or not evidence.startswith("/"):
                raise GateError("successful phase-1 result lacks durable evidence")
            print("phase1_roundtrip=PASS")
            print("factory_restore=verified")
            print(f"evidence={evidence}")
            return 0
        if state == "mavo_phase1_rolled_back":
            evidence = status.get("backup_path")
            if not isinstance(evidence, str) or not evidence.startswith("/"):
                raise GateError("rolled-back phase-1 result lacks durable evidence")
            print("phase1_roundtrip=FAILED_SAFE", file=sys.stderr)
            print("factory_restore=verified", file=sys.stderr)
            print(f"evidence={evidence}", file=sys.stderr)
            return 20
        if state == "mavo_phase1_recovery_required":
            print("phase1_roundtrip=RECOVERY_REQUIRED", file=sys.stderr)
            print("factory_restore=unverified", file=sys.stderr)
            if isinstance(status.get("backup_path"), str):
                print(f"evidence={status['backup_path']}", file=sys.stderr)
            return 21
        if state == "mavo_phase1_failed":
            print("phase1_roundtrip=STOPPED_BEFORE_WRITE", file=sys.stderr)
            print("factory_profile_unchanged=yes", file=sys.stderr)
            return 22
        if state != "mavo_phase1_running":
            raise GateError("backend returned an unexpected phase-1 state")
        time.sleep(0.01 if TESTING else 2)
    raise GateError("phase-1 status timed out; backend may still be restoring, do not unplug it")


try:
    preflight()
    if MODE == "apply":
        raise SystemExit(apply_roundtrip())
except GateError as exc:
    print(f"phase1_preflight=FAIL: {exc}", file=sys.stderr)
    raise SystemExit(1)
PY
