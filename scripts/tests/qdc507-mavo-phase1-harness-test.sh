#!/bin/bash

set -euo pipefail
umask 077

readonly ROOT="$(CDPATH= cd -P -- "$(dirname -- "$0")/../.." && pwd)"
readonly HARNESS="$ROOT/tools/qdc507-mavo-phase1.sh"
fixture_root="$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/mavo-phase1-test.XXXXXX")"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then
    /bin/kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "$fixture_root" == "${TMPDIR:-/tmp}"/mavo-phase1-test.* && -d "$fixture_root" ]]; then
    /bin/rm -rf -- "$fixture_root"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

fixture="$fixture_root/host.json"
state="$fixture_root/requests.jsonl"
port_file="$fixture_root/port"
printf '%s\n' '{"usb":[{"idVendor":11427,"idProduct":16390,"locationID":17825792,"iSerialNumber":0}],"audio":{"SPAudioDataType":[{"_items":[{"_name":"MacBook Pro Speakers"}]}]}}' > "$fixture"

start_server() {
  local terminal_state="$1"
  : > "$state"
  : > "$port_file"
  /usr/bin/python3 - "$state" "$port_file" "$terminal_state" <<'PY' &
import json
import pathlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

state_path, port_path, terminal_state = sys.argv[1:4]
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
    'AT+CLCC': 'AT+CLCC\r\n+CLCC: 1,1,0,1,0,"",128\r\n+CLCC: 2,1,0,1,0,"",128\r\nOK\r\n',
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
        if self.path != '/api/module/setup':
            return self.send_json(404, {'error': 'not found'})
        if getattr(self.server, 'started', False):
            return self.send_json(200, {'state': terminal_state, 'backup_path': '/private/evidence.json'})
        return self.send_json(200, {'state': 'needs_initialization'})
    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        body = json.loads(self.rfile.read(length) or b'{}')
        self.record(body)
        if self.path == '/api/at':
            command = body.get('command')
            if command not in responses:
                return self.send_json(403, {'error': 'unsafe'})
            return self.send_json(200, {'response': responses[command]})
        if self.path == '/api/module/setup':
            self.server.started = True
            return self.send_json(202, {'state': 'mavo_phase1_running'})
        return self.send_json(404, {'error': 'not found'})

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
}

run_harness() {
  DJONEHUB_PHASE1_TESTING=1 \
  DJONEHUB_PHASE1_TEST_FIXTURE="$fixture" \
  DJONEHUB_PHASE1_BASE_URL="http://127.0.0.1:$(<"$port_file")" \
    "$HARNESS" "$@"
}

start_server mavo_phase1_complete
check_output="$fixture_root/check.out"
run_harness > "$check_output"
/usr/bin/grep -Fq 'phase1_preflight=PASS' "$check_output"
/usr/bin/grep -Fq 'mutation_sent=no' "$check_output"
/usr/bin/grep -Fq 'identity_sha256=e27a7686b8028cfee7b57d954c3abccfb2a701968925f52bbd482e77be5de0bb' "$check_output"
! /usr/bin/grep -Fq '123456789012345' "$check_output"
! /usr/bin/grep -Fq '"experiment"' "$state"

before_wrong_confirm="$(/usr/bin/wc -l < "$state" | /usr/bin/tr -d ' ')"
if run_harness apply --confirm yes > /dev/null 2>&1; then
  printf '%s\n' 'harness accepted an inexact confirmation' >&2
  exit 1
fi
[[ "$before_wrong_confirm" == "$(/usr/bin/wc -l < "$state" | /usr/bin/tr -d ' ')" ]]

apply_output="$fixture_root/apply.out"
run_harness apply --confirm '一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号' > "$apply_output"
/usr/bin/grep -Fq 'phase1_roundtrip=PASS' "$apply_output"
/usr/bin/grep -Fq 'factory_restore=verified' "$apply_output"
/usr/bin/python3 - "$state" <<'PY'
import json
import sys
rows = [json.loads(line) for line in open(sys.argv[1])]
mutations = [row for row in rows if row['path'] == '/api/module/setup' and row['method'] == 'POST']
assert len(mutations) == 1
assert mutations[0]['body'] == {
    'confirm': True,
    'experiment': 'mavo_adb_uac_roundtrip',
    'phase1_confirmation': '一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号',
}
at_commands = [row['body']['command'] for row in rows if row['path'] == '/api/at']
assert at_commands
allowed = {
    'AT+QCFG="USBCFG"', 'AT+QCFG="usbnet"', 'AT+QCFG="ims"', 'AT+QCFG="volte_disable"',
    'AT+CPIN?', 'AT+CEREG?', 'AT+CGATT?', 'AT+CFUN?', 'AT+CGMR', 'AT+CGSN', 'AT+CLCC',
}
assert set(at_commands) == allowed
PY

/bin/kill "$server_pid"
wait "$server_pid" 2>/dev/null || true
server_pid=""
start_server mavo_phase1_rolled_back
if run_harness apply --confirm '一次 ADB+UAC profile 往返，自动恢复，不推模块、不拨号' > "$fixture_root/rolled-back.out" 2>&1; then
  printf '%s\n' 'harness treated a rolled-back experiment as success' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 20 ]]
/usr/bin/grep -Fq 'phase1_roundtrip=FAILED_SAFE' "$fixture_root/rolled-back.out"
/usr/bin/grep -Fq 'factory_restore=verified' "$fixture_root/rolled-back.out"

printf '%s\n' 'qdc507 MaVo phase-1 harness: PASS'
