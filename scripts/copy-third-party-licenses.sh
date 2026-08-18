#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST_ROOT=${1:?"usage: copy-third-party-licenses.sh DESTINATION_ROOT"}

if [ ! -d "${DEST_ROOT}" ]; then
  echo "License destination does not exist: ${DEST_ROOT}" >&2
  exit 66
fi

mkdir -p \
	"${DEST_ROOT}/licenses" \
  "${DEST_ROOT}/third_party/euicc-go/bertlv" \
  "${DEST_ROOT}/third_party/multierr" \
  "${DEST_ROOT}/third_party/pkg-errors" \
  "${DEST_ROOT}/third_party/quectel-qmi-go" \
  "${DEST_ROOT}/third_party/strftime" \
  "${DEST_ROOT}/third_party/uicc-go" \
  "${DEST_ROOT}/third_party/x-sys" \
	"${DEST_ROOT}/third_party/x-text"

cp "${ROOT_DIR}/licenses/MaVo-LICENSE" "${DEST_ROOT}/licenses/MaVo-LICENSE"
cp "${ROOT_DIR}/licenses/Pion-LICENSE" "${DEST_ROOT}/licenses/Pion-LICENSE"
cp "${ROOT_DIR}/licenses/Pion-logging-LICENSE" "${DEST_ROOT}/licenses/Pion-logging-LICENSE"
cp "${ROOT_DIR}/licenses/Pion-mdns-LICENSE" "${DEST_ROOT}/licenses/Pion-mdns-LICENSE"
cp "${ROOT_DIR}/licenses/Pion-randutil-LICENSE" "${DEST_ROOT}/licenses/Pion-randutil-LICENSE"
cp "${ROOT_DIR}/licenses/Pion-rtp-LICENSE" "${DEST_ROOT}/licenses/Pion-rtp-LICENSE"
cp "${ROOT_DIR}/third_party/euicc-go/LICENSE" "${DEST_ROOT}/third_party/euicc-go/LICENSE"
cp "${ROOT_DIR}/third_party/euicc-go/bertlv/LICENSE" "${DEST_ROOT}/third_party/euicc-go/bertlv/LICENSE"
cp "${ROOT_DIR}/third_party/multierr/LICENSE.txt" "${DEST_ROOT}/third_party/multierr/LICENSE.txt"
cp "${ROOT_DIR}/third_party/pkg-errors/LICENSE" "${DEST_ROOT}/third_party/pkg-errors/LICENSE"
cp "${ROOT_DIR}/third_party/quectel-qmi-go/LICENSE" "${DEST_ROOT}/third_party/quectel-qmi-go/LICENSE"
cp "${ROOT_DIR}/third_party/strftime/LICENSE" "${DEST_ROOT}/third_party/strftime/LICENSE"
cp "${ROOT_DIR}/third_party/uicc-go/LICENSE" "${DEST_ROOT}/third_party/uicc-go/LICENSE"
cp "${ROOT_DIR}/third_party/x-sys/LICENSE" "${DEST_ROOT}/third_party/x-sys/LICENSE"
cp "${ROOT_DIR}/third_party/x-text/LICENSE" "${DEST_ROOT}/third_party/x-text/LICENSE"
