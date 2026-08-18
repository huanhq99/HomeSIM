#!/bin/bash

set -euo pipefail
umask 077

readonly ROOT="$(CDPATH= cd -P -- "$(dirname -- "$0")/../.." && pwd)"
readonly HARNESS="$ROOT/tools/qdc507-mavo-phase2.sh"
readonly CONFIRMATION='一次 MaVo Phase-2 临时运行时探测，结束后卸载清理并恢复，不拨号不发短信'

fixture_root="$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/mavo-phase2-test.XXXXXX")"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then
    /bin/kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "$fixture_root" == "${TMPDIR:-/tmp}"/mavo-phase2-test.* && -d "$fixture_root" ]]; then
    /bin/rm -rf -- "$fixture_root"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

runtime_cache="$fixture_root/runtime"
/bin/mkdir -m 700 "$runtime_cache"
printf 'phase2-test-helper\n' > "$runtime_cache/mavo-pcm-bridge.armv7"
printf 'phase2-test-apr\n' > "$runtime_cache/qdc507_aprv3.ko"
printf 'phase2-test-voice\n' > "$runtime_cache/qdc507_voice.ko"
/bin/chmod 700 "$runtime_cache/mavo-pcm-bridge.armv7"
/bin/chmod 600 "$runtime_cache/qdc507_aprv3.ko" "$runtime_cache/qdc507_voice.ko"

host_fixture="$fixture_root/host.json"
request_log="$fixture_root/requests.jsonl"
port_file="$fixture_root/port"
printf '%s\n' '{"usb":[{"idVendor":11427,"idProduct":16390,"locationID":17825792,"iSerialNumber":0}],"audio":{"SPAudioDataType":[{"_items":[{"_name":"MacBook Pro Speakers"}]}]}}' > "$host_fixture"
: > "$request_log"
: > "$port_file"

/usr/bin/python3 - "$request_log" "$port_file" <<'PY' &
import json
import pathlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

state_path, port_path = sys.argv[1:3]
responses = {
    'AT+QCFG="USBCFG"': 'AT+QCFG="USBCFG"\r\n+QCFG: "usbcfg",0x2ca3,0x4006,1,1,1,1,1,0,0\r\nOK\r\n',
    'AT+QCFG="usbnet"': 'AT+QCFG="usbnet"\r\n+QCFG: "usbnet",0\r\nOK\r\n',
    'AT+QCFG="ims"': 'AT+QCFG="ims"\r\n+QCFG: "ims",0,1\r\nOK\r\n',
    'AT+QCFG="volte_disable"': 'AT+QCFG="volte_disable"\r\n+QCFG: "volte_disable",0\r\nOK\r\n',
    'AT+CPIN?': 'AT+CPIN?\r\n+CPIN: READY\r\nOK\r\n',
    'AT+CEREG?': 'AT+CEREG?\r\n+CEREG: 2,1\r\nOK\r\n',
    'AT+CGATT?': 'AT+CGATT?\r\n+CGATT: 1\r\nOK\r\n',
    'AT+CFUN?': 'AT+CFUN?\r\n+CFUN: 1\r\nOK\r\n',
    'AT+CGMR': 'AT+CGMR\r\nQDC507GLEFM21\r\nOK\r\n',
    'AT+CGSN': 'AT+CGSN\r\n123456789012345\r\nOK\r\n',
    'AT+CLCC': 'AT+CLCC\r\n+CLCC: 1,1,0,1,0,"",128\r\nOK\r\n',
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send_json(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def record(self, body):
        with open(state_path, 'a') as handle:
            handle.write(json.dumps({'method': self.command, 'path': self.path, 'body': body}) + '\n')

    def do_GET(self):
        self.record(None)
        if self.path == '/api/module/setup':
            return self.send_json(200, {'state': 'needs_initialization'})
        return self.send_json(404, {'error': 'not found'})

    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        body = json.loads(self.rfile.read(length) or b'{}')
        self.record(body)
        if self.path == '/api/at' and body.get('command') in responses:
            return self.send_json(200, {'response': responses[body['command']]})
        return self.send_json(403, {'error': 'unsafe'})


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
pathlib.Path(port_path).write_text(str(server.server_address[1]))
server.serve_forever()
PY
server_pid=$!
for _ in {1..100}; do
  [[ -s "$port_file" ]] && break
  /bin/sleep 0.01
done
[[ -s "$port_file" ]]

run_harness() {
  DJONEHUB_PHASE2_TESTING=1 \
  DJONEHUB_PHASE2_TEST_CACHE="$runtime_cache" \
  DJONEHUB_PHASE1_TESTING=1 \
  DJONEHUB_PHASE1_TEST_FIXTURE="$host_fixture" \
  DJONEHUB_PHASE1_BASE_URL="http://127.0.0.1:$(<"$port_file")" \
    "$HARNESS" "$@"
}

check_output="$fixture_root/check.out"
run_harness > "$check_output"
/usr/bin/grep -Fq 'phase2_cache=PASS' "$check_output"
/usr/bin/grep -Fq 'runtime_files=3' "$check_output"
[[ "$(/usr/bin/grep -c '^runtime_artifact=' "$check_output")" == 3 ]]
/usr/bin/grep -Fq 'phase1_preflight=PASS' "$check_output"
/usr/bin/grep -Fq 'phase2_preflight=PASS' "$check_output"
/usr/bin/grep -Fq 'mutation_sent=no' "$check_output"
/usr/bin/grep -Fq 'call_command_sent=no' "$check_output"
/usr/bin/grep -Fq 'sms_mutation_sent=no' "$check_output"
! /usr/bin/grep -Fq '123456789012345' "$check_output"

/usr/bin/python3 - "$request_log" <<'PY'
import json
import sys

rows = [json.loads(line) for line in open(sys.argv[1])]
assert rows
assert not any(row['path'] == '/api/module/setup' and row['method'] == 'POST' for row in rows)
allowed = {
    'AT+QCFG="USBCFG"', 'AT+QCFG="usbnet"', 'AT+QCFG="ims"', 'AT+QCFG="volte_disable"',
    'AT+CPIN?', 'AT+CEREG?', 'AT+CGATT?', 'AT+CFUN?', 'AT+CGMR', 'AT+CGSN', 'AT+CLCC',
}
commands = [row['body']['command'] for row in rows if row['path'] == '/api/at']
assert commands and set(commands) == allowed
PY

before_wrong="$(/usr/bin/wc -l < "$request_log" | /usr/bin/tr -d ' ')"
if run_harness apply --confirm yes > /dev/null 2>&1; then
  printf '%s\n' 'Phase-2 harness accepted an inexact confirmation' >&2
  exit 1
fi
[[ "$before_wrong" == "$(/usr/bin/wc -l < "$request_log" | /usr/bin/tr -d ' ')" ]]

apply_output="$fixture_root/apply.out"
if run_harness apply --confirm "$CONFIRMATION" > "$apply_output" 2>&1; then
  printf '%s\n' 'Phase-2 harness unexpectedly executed apply' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 23 ]]
/usr/bin/grep -Fq 'phase2_apply=LOCKED_NOT_AUTHORIZED' "$apply_output"
/usr/bin/grep -Fq 'live_adapter_wired=no' "$apply_output"
/usr/bin/grep -Fq 'mutation_sent=no' "$apply_output"

/usr/bin/python3 - "$request_log" <<'PY'
import json
import sys

rows = [json.loads(line) for line in open(sys.argv[1])]
assert not any(row['path'] == '/api/module/setup' and row['method'] == 'POST' for row in rows)
assert all(row['method'] == 'GET' or row['path'] == '/api/at' for row in rows)
PY

before_tamper="$(/usr/bin/wc -l < "$request_log" | /usr/bin/tr -d ' ')"
printf 'tampered\n' > "$runtime_cache/qdc507_voice.ko"
if run_harness check > "$fixture_root/tamper.out" 2>&1; then
  printf '%s\n' 'Phase-2 harness accepted a tampered cache artifact' >&2
  exit 1
fi
/usr/bin/grep -Fq 'failed fixed SHA-256 verification' "$fixture_root/tamper.out"
[[ "$before_tamper" == "$(/usr/bin/wc -l < "$request_log" | /usr/bin/tr -d ' ')" ]]

for forbidden in 'AT+QCFG="USBCFG",' 'AT+CFUN=1,1' 'insmod ' 'rmmod ' '/api/module/setup"' ; do
  if /usr/bin/grep -Fq "$forbidden" "$HARNESS"; then
    printf 'Phase-2 shell contains a live mutation marker: %s\n' "$forbidden" >&2
    exit 1
  fi
done

printf '%s\n' 'qdc507 MaVo phase-2 harness: PASS'

