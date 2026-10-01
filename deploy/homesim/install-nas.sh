#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root_dir"
bind_ip="${1:-127.0.0.1}"
tty_path="${2:-/dev/serial/by-id/usb-BAIWANG_Baiwang-if02-port0}"
[[ "$bind_ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid LAN bind IP'; exit 1; }
[[ -c "$tty_path" ]] || { echo 'The selected serial device does not exist'; exit 1; }
[[ -x deploy/homesim/homesim ]] || { echo 'Missing locally built HomeSIM executable'; exit 1; }
docker compose version
if docker container inspect homesim >/dev/null 2>&1; then
  echo 'HomeSIM already exists. Upgrade explicitly after backing up data.'
  exit 1
fi
umask 077
if [[ -e .env ]]; then
  grep -Fxq "HOMESIM_TTY=$tty_path" .env && grep -Fxq "HOMESIM_BIND_IP=$bind_ip" .env || { echo 'Existing .env differs. Review before retrying.'; exit 1; }
else
  printf 'HOMESIM_TTY=%s\nHOMESIM_AUDIO_DEVICE=hw:Baiwang,0\nHOMESIM_BIND_IP=%s\nHOMESIM_TRUST_PROXY=0\n' "$tty_path" "$bind_ip" > .env
fi
mkdir -p data
chmod 700 data
cp LICENSE NOTICE THIRD_PARTY_NOTICES.md deploy/homesim/
docker compose -f compose.yaml -f compose.prebuilt.yaml config -q
# Build before changing host service state.
docker compose -f compose.yaml -f compose.prebuilt.yaml build --build-arg RUNTIME_IMAGE="${HOMESIM_RUNTIME_IMAGE:-debian:bookworm-slim}" --build-arg DEBIAN_MIRROR="${HOMESIM_DEBIAN_MIRROR:-deb.debian.org}"
mm_was_active=0
if systemctl is-active --quiet ModemManager.service; then
  mm_was_active=1
  systemctl stop ModemManager.service
fi
rollback() {
  status=$?
  if [[ "$status" -ne 0 && "$mm_was_active" -eq 1 ]]; then
    systemctl start ModemManager.service || true
  fi
}
trap rollback EXIT
printf 'modemmanager_was_active=%s\n' "$mm_was_active" > data/install-state
HOMESIM_TTY="$tty_path" deploy/homesim/homesim probe
# Narrow ignore rule for this Baiwang USB modem. It does not disable the
# ModemManager service, radio or NAS network interfaces.
if [[ "$mm_was_active" -eq 1 ]]; then
  rule_path=/etc/udev/rules.d/79-homesim-baiwang.rules
  if [[ -e "$rule_path" ]]; then
    echo 'An existing HomeSIM udev rule was found; review it first.'
    exit 1
  fi
  printf '%s\n' 'ACTION!="remove", SUBSYSTEM=="tty", ATTRS{idVendor}=="2c7c", ATTRS{idProduct}=="0125", ATTRS{product}=="Baiwang", ENV{ID_MM_DEVICE_IGNORE}="1", ENV{ID_MM_PORT_IGNORE}="1"' > "$rule_path"
  udevadm control --reload-rules
  udevadm trigger --subsystem-match=tty
fi
docker compose -f compose.yaml -f compose.prebuilt.yaml up -d
if [[ "$mm_was_active" -eq 1 ]]; then systemctl start ModemManager.service; fi
trap - EXIT
docker compose -f compose.yaml -f compose.prebuilt.yaml ps
printf 'HomeSIM URL: http://%s:8580/\n' "$bind_ip"
echo 'Read data/setup-token privately in your NAS terminal to create your account.'
