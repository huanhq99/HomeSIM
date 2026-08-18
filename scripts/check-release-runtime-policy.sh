#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
Usage:
  scripts/check-release-runtime-policy.sh PATH [PATH ...]
  scripts/check-release-runtime-policy.sh --git-tracked [REPOSITORY]

Reject module-side voice runtime files from release directories, supported
archives, and the Git index. Supported archives are ZIP, DMG, tar, tar.gz,
tgz, and tar.bz2; nested archives are inspected recursively. Matching is by
path/name, symlink target, and the SHA-256 of the three pinned executable
payloads.
EOF
}

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
esac

if [ "$#" -eq 0 ]; then
  usage >&2
  exit 64
fi

CHECK_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-runtime-policy.XXXXXX")
MOUNT_POINTS_FILE="${TMP_ROOT}/mounted-dmgs.txt"
: >"${MOUNT_POINTS_FILE}"

MAX_ARCHIVE_DEPTH=${MACCELLULAR_RUNTIME_POLICY_MAX_ARCHIVE_DEPTH:-4}
MAX_EXPANDED_FILES=${MACCELLULAR_RUNTIME_POLICY_MAX_EXPANDED_FILES:-20000}
MAX_EXPANDED_BYTES=${MACCELLULAR_RUNTIME_POLICY_MAX_EXPANDED_BYTES:-1073741824}

for limit_name in MAX_ARCHIVE_DEPTH MAX_EXPANDED_FILES MAX_EXPANDED_BYTES; do
  eval "limit_value=\${${limit_name}}"
  case "$limit_value" in
    ''|*[!0-9]*)
      printf '%s must be a non-negative integer.\n' "$limit_name" >&2
      exit 64
      ;;
  esac
done

ARCHIVE_SERIAL=0
EXPANDED_FILE_COUNT=0
EXPANDED_BYTE_COUNT=0

cleanup() {
  local mount_point
  while IFS= read -r mount_point; do
    [ -n "$mount_point" ] || continue
    hdiutil detach "$mount_point" -quiet >/dev/null 2>&1 || true
  done <"${MOUNT_POINTS_FILE}"
  /bin/rm -rf -- "${TMP_ROOT}"
}
trap cleanup EXIT HUP INT TERM

is_forbidden_path() {
  local lower_path
  lower_path=$(printf '%s' "$1" | /usr/bin/tr '[:upper:]' '[:lower:]')
  case "$lower_path" in
    modulevoice|modulevoice/*|*/modulevoice|*/modulevoice/*|\
    *.ko|*.armv7|\
    mavo-pcm-bridge.armv7|*/mavo-pcm-bridge.armv7|\
    qdc507_aprv3.ko|*/qdc507_aprv3.ko|\
    qdc507_voice.ko|*/qdc507_voice.ko)
      return 0
      ;;
  esac
  return 1
}

is_forbidden_hash() {
  case "$1" in
    88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc|\
    3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a|\
    ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c)
      return 0
      ;;
  esac
  return 1
}

FAILURES=0

reject() {
  local source detail
  source=$1
  detail=$2
  printf 'FORBIDDEN module-side runtime in %s: %s\n' "$source" "$detail" >&2
  FAILURES=$((FAILURES + 1))
}

scan_file() {
  local display path depth hash
  display=$1
  path=$2
  depth=$3
  hash=$(/usr/bin/shasum -a 256 "$path" | /usr/bin/awk '{print $1}')
  if is_forbidden_hash "$hash"; then
    reject "$display" "payload SHA-256 ${hash}"
  fi
  archive_kind "$path"
  if [ -n "$ARCHIVE_KIND" ]; then
    scan_archive "$path" "$display" "$depth" "$ARCHIVE_KIND"
  fi
}

scan_directory() {
  local root label depth count_expansion list path relative target resolved
  root=$(cd "$1" && pwd -P)
  label=$2
  depth=$3
  count_expansion=${4:-0}
  if [ "$count_expansion" -eq 1 ]; then
    if ! account_expanded_tree "$root" "$label"; then
      return 0
    fi
  fi
  ARCHIVE_SERIAL=$((ARCHIVE_SERIAL + 1))
  list="${TMP_ROOT}/directory-list.${ARCHIVE_SERIAL}.txt"
  find "$root" -mindepth 1 -print >"$list"
  while IFS= read -r path; do
    relative=${path#"$root"/}
    if is_forbidden_path "$relative"; then
      reject "$label" "$relative"
      continue
    fi
    if [ -L "$path" ]; then
      target=$(readlink "$path")
      if is_forbidden_path "$target"; then
        reject "$label" "$relative -> $target"
        continue
      fi
      resolved=$(perl -MCwd=realpath -e 'print realpath($ARGV[0]) // q{}' "$path" 2>/dev/null || true)
      case "$resolved" in
        "$root"|"$root"/*) ;;
        *)
          reject "$label" "$relative -> $target (escapes release root)"
          continue
          ;;
      esac
    fi
    if [ -f "$path" ]; then
      scan_file "$label/$relative" "$path" "$depth"
    fi
  done <"$list"
}

account_expanded_tree() {
  local root label entry_count byte_count next_file_count next_byte_count
  root=$1
  label=$2
  entry_count=$(find "$root" -mindepth 1 -print | /usr/bin/awk 'END { print NR + 0 }')
  byte_count=$(find "$root" -type f -exec /usr/bin/stat -f '%z' {} + | /usr/bin/awk '{ total += $1 } END { print total + 0 }')
  next_file_count=$((EXPANDED_FILE_COUNT + entry_count))
  next_byte_count=$((EXPANDED_BYTE_COUNT + byte_count))
  if [ "$next_file_count" -gt "$MAX_EXPANDED_FILES" ]; then
    reject "$label" "expanded entry limit exceeded (${next_file_count} > ${MAX_EXPANDED_FILES})"
    return 1
  fi
  if [ "$next_byte_count" -gt "$MAX_EXPANDED_BYTES" ]; then
    reject "$label" "expanded byte limit exceeded (${next_byte_count} > ${MAX_EXPANDED_BYTES})"
    return 1
  fi
  EXPANDED_FILE_COUNT=$next_file_count
  EXPANDED_BYTE_COUNT=$next_byte_count
  return 0
}

preflight_expansion_budget() {
  local entry_count byte_count label next_file_count next_byte_count
  entry_count=$1
  byte_count=$2
  label=$3
  next_file_count=$((EXPANDED_FILE_COUNT + entry_count))
  next_byte_count=$((EXPANDED_BYTE_COUNT + byte_count))
  if [ "$next_file_count" -gt "$MAX_EXPANDED_FILES" ]; then
    reject "$label" "archive would exceed expanded entry limit (${next_file_count} > ${MAX_EXPANDED_FILES})"
    return 1
  fi
  if [ "$next_byte_count" -gt "$MAX_EXPANDED_BYTES" ]; then
    reject "$label" "archive would exceed expanded byte limit (${next_byte_count} > ${MAX_EXPANDED_BYTES})"
    return 1
  fi
  return 0
}

archive_kind() {
  local lower_name
  lower_name=$(printf '%s' "$1" | /usr/bin/tr '[:upper:]' '[:lower:]')
  case "$lower_name" in
    *.tar.gz|*.tgz) ARCHIVE_KIND=targz ;;
    *.tar.bz2) ARCHIVE_KIND=tarbz2 ;;
    *.tar) ARCHIVE_KIND=tar ;;
    *.zip) ARCHIVE_KIND=zip ;;
    *.dmg) ARCHIVE_KIND=dmg ;;
    *) ARCHIVE_KIND='' ;;
  esac
}

archive_members_are_safe() {
  local list label member
  list=$1
  label=$2
  while IFS= read -r member; do
    case "$member" in
      ''|.) continue ;;
      /*|..|../*|*/..|*/../*)
        reject "$label" "archive member escapes extraction root: $member"
        return 1
        ;;
    esac
  done <"$list"
  return 0
}

scan_archive() {
  local archive label depth kind archive_root member_list metadata_list size_list
  local entry_count byte_count mount_point
  archive=$1
  label=$2
  depth=$3
  kind=$4
  if [ "$depth" -ge "$MAX_ARCHIVE_DEPTH" ]; then
    reject "$label" "nested archive depth limit exceeded (${depth} >= ${MAX_ARCHIVE_DEPTH})"
    return 0
  fi

  ARCHIVE_SERIAL=$((ARCHIVE_SERIAL + 1))
  archive_root="${TMP_ROOT}/archive-${ARCHIVE_SERIAL}"
  member_list="${TMP_ROOT}/archive-members-${ARCHIVE_SERIAL}.txt"
  metadata_list="${TMP_ROOT}/archive-metadata-${ARCHIVE_SERIAL}.txt"
  size_list="${TMP_ROOT}/archive-sizes-${ARCHIVE_SERIAL}.txt"
  mkdir -p "$archive_root"

  case "$kind" in
    zip)
      if ! /usr/bin/unzip -Z1 "$archive" >"$member_list" 2>/dev/null; then
        reject "$label" "cannot list ZIP archive safely"
        return 0
      fi
      if ! archive_members_are_safe "$member_list" "$label"; then return 0; fi
      if ! /usr/bin/zipinfo -l "$archive" >"$metadata_list" 2>/dev/null; then
        reject "$label" "cannot read ZIP expansion metadata"
        return 0
      fi
      if /usr/bin/awk 'substr($1, 1, 1) == "l" { found = 1 } END { exit !found }' "$metadata_list"; then
        reject "$label" "ZIP contains a symbolic-link entry"
        return 0
      fi
      if ! /usr/bin/unzip -l "$archive" >"$size_list" 2>/dev/null; then
        reject "$label" "cannot read ZIP expansion sizes"
        return 0
      fi
      entry_count=$(/usr/bin/awk 'END { print NR + 0 }' "$member_list")
      byte_count=$(/usr/bin/awk '$1 ~ /^[0-9]+$/ && index($2, "-") > 0 { total += $1 } END { print total + 0 }' "$size_list")
      if ! preflight_expansion_budget "$entry_count" "$byte_count" "$label"; then return 0; fi
      if ! /usr/bin/ditto -x -k "$archive" "$archive_root" >/dev/null 2>&1; then
        reject "$label" "cannot extract ZIP archive safely"
        return 0
      fi
      ;;
    tar|targz|tarbz2)
      if ! /usr/bin/tar -tf "$archive" >"$member_list" 2>/dev/null; then
        reject "$label" "cannot list tar archive safely"
        return 0
      fi
      if ! archive_members_are_safe "$member_list" "$label"; then return 0; fi
      if ! /usr/bin/tar -tvf "$archive" >"$metadata_list" 2>/dev/null; then
        reject "$label" "cannot read tar expansion metadata"
        return 0
      fi
      if /usr/bin/awk 'substr($1, 1, 1) == "l" || substr($1, 1, 1) == "h" { found = 1 } END { exit !found }' "$metadata_list"; then
        reject "$label" "tar contains a symbolic-link or hard-link entry"
        return 0
      fi
      entry_count=$(/usr/bin/awk 'END { print NR + 0 }' "$member_list")
      byte_count=$(/usr/bin/awk 'substr($1, 1, 1) == "-" && $5 ~ /^[0-9]+$/ { total += $5 } END { print total + 0 }' "$metadata_list")
      if ! preflight_expansion_budget "$entry_count" "$byte_count" "$label"; then return 0; fi
      if ! /usr/bin/tar -xf "$archive" -C "$archive_root" >/dev/null 2>&1; then
        reject "$label" "cannot extract tar archive safely"
        return 0
      fi
      ;;
    dmg)
      mount_point="${archive_root}/mount"
      mkdir -p "$mount_point"
      printf '%s\n' "$mount_point" >>"${MOUNT_POINTS_FILE}"
      if ! hdiutil attach "$archive" -readonly -nobrowse -mountpoint "$mount_point" -quiet; then
        reject "$label" "cannot attach DMG read-only"
        return 0
      fi
      scan_directory "$mount_point" "$label" "$((depth + 1))" 1
      hdiutil detach "$mount_point" -quiet >/dev/null 2>&1 || reject "$label" "cannot detach inspected DMG"
      return 0
      ;;
  esac

  scan_directory "$archive_root" "$label" "$((depth + 1))" 1
}

scan_git_tracked() {
  local repo index_root
  repo=$1
  git -C "$repo" rev-parse --is-inside-work-tree >/dev/null
  index_root="${TMP_ROOT}/git-index"
  mkdir -p "$index_root"
  git -C "$repo" checkout-index --all --prefix="${index_root}/"
  scan_directory "$index_root" "Git index" 0 0
}

if [ "${1:-}" = "--git-tracked" ]; then
  [ "$#" -le 2 ] || { usage >&2; exit 64; }
  scan_git_tracked "${2:-$CHECK_ROOT}"
else
  for target in "$@"; do
    if [ -d "$target" ]; then
      scan_directory "$(CDPATH= cd -- "$target" && pwd)" "$target" 0 0
    elif [ -f "$target" ]; then
      if is_forbidden_path "$(basename -- "$target")"; then
        reject "$target" "forbidden file name"
      else
        scan_file "$target" "$target" 0
      fi
    else
      printf 'Cannot inspect missing path: %s\n' "$target" >&2
      exit 66
    fi
  done
fi

if [ "$FAILURES" -ne 0 ]; then
  printf 'Release runtime policy: FAIL (%s finding(s))\n' "$FAILURES" >&2
  exit 1
fi

printf 'Release runtime policy: PASS\n'
