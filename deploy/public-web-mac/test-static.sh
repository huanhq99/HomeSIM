#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -P -- "$(dirname -- "$0")" && pwd)

/usr/bin/plutil -lint \
  "$ROOT/io.maccellular.phone.plist.example" \
  "$ROOT/io.maccellular.phone.direct-voice.plist.example" \
  "$ROOT/io.maccellular.phone.external-sip.plist.example" \
  "$ROOT/io.maccellular.phone.media-helper.plist.example" \
  "$ROOT/io.maccellular.phone.cloudflared.plist.example" >/dev/null

/bin/bash -n \
  "$ROOT/install-local.sh" \
  "$ROOT/tests/test-install-local.sh" \
  "$ROOT/tests/fixtures/fake-launchctl.sh" \
  "$ROOT/tests/fixtures/fake-curl.sh" \
  "$ROOT/tests/fixtures/fake-sleep.sh"

/usr/bin/python3 - "$ROOT" <<'PY'
import json
import pathlib
import plistlib
import sys

root = pathlib.Path(sys.argv[1])
route = json.loads((root / "route-contract.json").read_text())
assert route == {
    "hostname": "phone.example.com",
    "service": "http://127.0.0.1:7578",
    "access_required": True,
    "purpose": "communications-pwa",
}

with (root / "io.maccellular.phone.plist.example").open("rb") as handle:
    backend = plistlib.load(handle)
backend_args = backend["ProgramArguments"]
assert backend_args[:3] == [
    "/usr/bin/caffeinate", "-s", "__DJONEHUB_BINARY__",
]
required_pairs = {
    "-listen": "127.0.0.1:7576",
    "-public-web-listen": "127.0.0.1:7578",
    "-public-web-host": "__PUBLIC_WEB_HOST__",
    "-public-web-access-team-domain": "__CLOUDFLARE_TEAM_DOMAIN__",
    "-public-web-access-audience": "__CLOUDFLARE_ACCESS_AUD__",
    "-public-web-access-allowed-email-file": "__ALLOWED_EMAILS_FILE__",
    "-sms-store": "__SMS_STORE_DIR__",
}
for flag, value in required_pairs.items():
    index = backend_args.index(flag)
    assert backend_args[index + 1] == value
assert "-public-web-control" in backend_args
assert "-sms-only-runtime" in backend_args
assert "-usb-at-only" in backend_args
assert "-port" not in backend_args
assert "-public-web-push" not in backend_args
assert "-public-web-push-vapid-private-key-file" not in backend_args
assert "-public-web-push-vapid-subject" not in backend_args
assert "-public-web-push-subscriptions-file" not in backend_args
assert not any("7577" in value for value in backend_args)

with (root / "io.maccellular.phone.direct-voice.plist.example").open("rb") as handle:
    direct_backend = plistlib.load(handle)
direct_args = direct_backend["ProgramArguments"]
assert direct_backend["Label"] == backend["Label"]
assert direct_args[:3] == [
    "/usr/bin/caffeinate", "-s", "__DJONEHUB_BINARY__",
]
for flag, value in required_pairs.items():
    index = direct_args.index(flag)
    assert direct_args[index + 1] == value
direct_pairs = {
    "-public-web-turn-host": "__TURN_HOST__",
    "-public-web-turn-secret-file": "__TURN_SECRET_FILE__",
    "-public-web-turn-udp-port": "3478",
    "-public-web-turn-tls-port": "443",
    "-public-web-turn-credential-ttl": "5m",
    "-public-web-push-vapid-private-key-file": "__VAPID_PRIVATE_KEY_FILE__",
    "-public-web-push-vapid-subject": "__PUSH_SUBJECT__",
    "-public-web-push-subscriptions-file": "__PUSH_SUBSCRIPTIONS_FILE__",
}
for flag, value in direct_pairs.items():
    index = direct_args.index(flag)
    assert direct_args[index + 1] == value
assert "-public-web-control" in direct_args
assert "-public-web-direct-voice" in direct_args
assert "-public-web-push" in direct_args
assert "-phone-relay-runtime" in direct_args
assert "-sms-only-runtime" not in direct_args
assert "-public-web-external-voice" not in direct_args
assert "-usb-at-only" in direct_args
assert "-port" not in direct_args
assert not any("7577" in value for value in direct_args)

with (root / "io.maccellular.phone.external-sip.plist.example").open("rb") as handle:
    external_backend = plistlib.load(handle)
external_args = external_backend["ProgramArguments"]
assert external_backend["Label"] == backend["Label"]
assert external_args[:3] == [
    "/usr/bin/caffeinate", "-s", "__DJONEHUB_BINARY__",
]
for flag, value in required_pairs.items():
    index = external_args.index(flag)
    assert external_args[index + 1] == value
external_pairs = {
    "-public-web-turn-host": "__TURN_HOST__",
    "-public-web-turn-secret-file": "__TURN_SECRET_FILE__",
    "-public-web-turn-udp-port": "3478",
    "-public-web-turn-tls-port": "443",
    "-public-web-turn-credential-ttl": "5m",
    "-public-web-push-vapid-private-key-file": "__VAPID_PRIVATE_KEY_FILE__",
    "-public-web-push-vapid-subject": "__PUSH_SUBJECT__",
    "-public-web-push-subscriptions-file": "__PUSH_SUBSCRIPTIONS_FILE__",
    "-voice-provider": "asterisk",
    "-voice-gateway-id": "__VOICE_GATEWAY_ID__",
    "-asterisk-ari-password-file": "__ASTERISK_CONTROL_PASSWORD_FILE__",
    "-asterisk-recovery-ari-password-file": "__ASTERISK_INSPECT_PASSWORD_FILE__",
    "-sip-recovery-store": "__SIP_RECOVERY_STORE__",
    "-sip-media-udp-min": "__SIP_MEDIA_UDP_MIN__",
    "-sip-media-udp-max": "__SIP_MEDIA_UDP_MAX__",
}
for flag, value in external_pairs.items():
    index = external_args.index(flag)
    assert external_args[index + 1] == value
assert "-public-web-control" in external_args
assert "-public-web-external-voice" in external_args
assert "-public-web-push" in external_args
assert "-sms-only-runtime" in external_args
assert "-sip-answer-incoming" in external_args
assert "-sip-reject-incoming" in external_args
assert "-sip-send-dtmf" in external_args
assert "-sip-end-active" in external_args
assert "-public-web-direct-voice" not in external_args
assert "-phone-relay-runtime" not in external_args
assert "-usb-at-only" in external_args
assert "-port" not in external_args
assert not any("7577" in value for value in external_args)
assert not any("DJOneHubNotifier" in value for value in external_args)

with (root / "io.maccellular.phone.media-helper.plist.example").open("rb") as handle:
    helper = plistlib.load(handle)
assert helper["Label"] == "io.maccellular.phone.media-helper"
assert helper["ProgramArguments"] == [
    "__MEDIA_HELPER_BINARY__",
    "--remote-media-helper",
    "--base-url",
    "http://127.0.0.1:7576/",
]
assert helper["RunAtLoad"] is True
assert helper["KeepAlive"] is True

with (root / "io.maccellular.phone.cloudflared.plist.example").open("rb") as handle:
    tunnel = plistlib.load(handle)
tunnel_args = tunnel["ProgramArguments"]
assert tunnel_args[0] == "__CLOUDFLARED_BINARY__"
assert tunnel_args[-2:] == ["--token-file", "__CLOUDFLARED_TOKEN_FILE__"]
assert not any(any(port in value for port in ("7576", "7577", "8088", "8089")) for value in tunnel_args)

for path in root.iterdir():
    if path.is_file() and path.name != "test-static.sh":
        data = path.read_bytes()
        assert b"CF-Access-Client-Secret" not in data
        assert b"TUNNEL_TOKEN=" not in data
PY

"$ROOT/tests/test-install-local.sh"

printf '%s\n' 'public web Mac static contract: PASS'
