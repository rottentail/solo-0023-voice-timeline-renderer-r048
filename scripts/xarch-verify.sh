#!/usr/bin/env bash
#
# xarch-verify.sh - repeatable cross-architecture audio compatibility check
# for the voice-timeline renderer.
#
# It renders the SAME digest-pinned input suite through two independently
# compiled voiceservice binaries (arm64 and amd64), then judges the downloaded
# outputs and receipts with the production-independent xarchcheck judge.
#
# Architecture execution modes:
#   - A binary runs natively when the host GOARCH matches it.
#   - Otherwise the script looks for a qemu user emulator and runs the foreign
#     binary under it.
#   - If neither native execution nor an emulator is available for an
#     architecture, that side is recorded as UNVERIFIED. The script never fakes
#     a result: a one-sided run exits 3 and writes an explicit conclusion.
#
# Outputs are kept under one result directory, including a self-contained
# failure-case/ whenever the judge finds a meaning-changing difference.
#
# Environment overrides:
#   GO                 go tool to use (default: go on PATH)
#   QEMU_AMD64         qemu emulator for amd64 binaries
#   QEMU_ARM64         qemu emulator for arm64 binaries
#   XARCH_RESULT_DIR   result directory (default: ./.xarch-results)
#   XARCH_FIXTURE_DIR  fixture directory (default: <result>/fixtures)
#   KEEP_WORK          set to 1 to retain per-arch service data directories
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
RESULT_DIR="${XARCH_RESULT_DIR:-${ROOT_DIR}/.xarch-results}"
FIXTURE_DIR="${XARCH_FIXTURE_DIR:-${RESULT_DIR}/fixtures}"
BIN_DIR="${RESULT_DIR}/bin"
WORK_DIR="${RESULT_DIR}/work"
HOST_ARCH="$(uname -m)"

# Map uname machine names to GOARCH labels.
case "${HOST_ARCH}" in
  x86_64 | amd64) HOST_GOARCH="amd64" ;;
  aarch64 | arm64) HOST_GOARCH="arm64" ;;
  *) HOST_GOARCH="unknown" ;;
esac

log() { printf '[xarch] %s\n' "$*"; }
die() { printf '[xarch] ERROR: %s\n' "$*" >&2; exit 1; }

mkdir -p "${RESULT_DIR}" "${BIN_DIR}" "${WORK_DIR}"

# Resolve an emulator candidate, honoring explicit env overrides first.
find_emulator() {
  local arch="$1"
  case "${arch}" in
    amd64)
      for candidate in "${QEMU_AMD64:-}" qemu-x86_64-static qemu-x86_64; do
        [ -n "${candidate}" ] && command -v "${candidate}" >/dev/null 2>&1 && { command -v "${candidate}"; return 0; }
      done
      ;;
    arm64)
      for candidate in "${QEMU_ARM64:-}" qemu-aarch64-static qemu-aarch64; do
        [ -n "${candidate}" ] && command -v "${candidate}" >/dev/null 2>&1 && { command -v "${candidate}"; return 0; }
      done
      ;;
  esac
  return 1
}

# Decide how an architecture binary can be executed on this host.
# Prints "native", "qemu:<path>", or "unavailable".
execution_mode() {
  local arch="$1"
  if [ "${HOST_GOARCH}" = "${arch}" ]; then
    printf 'native'
    return 0
  fi
  if emulator="$(find_emulator "${arch}")"; then
    printf 'qemu:%s' "${emulator}"
    return 0
  fi
  printf 'unavailable'
}

log "host architecture: ${HOST_ARCH} (${HOST_GOARCH})"

# 1. Build the judge/runner tool and both architecture service binaries.
log "building xarchcheck and voiceservice for arm64 + amd64"
( cd "${ROOT_DIR}" && "${GO}" build -buildvcs=false -o "${BIN_DIR}/xarchcheck" ./cmd/xarchcheck )
( cd "${ROOT_DIR}" && GOOS=linux GOARCH=arm64 "${GO}" build -buildvcs=false -o "${BIN_DIR}/voiceservice-arm64" ./cmd/voiceservice )
( cd "${ROOT_DIR}" && GOOS=linux GOARCH=amd64 "${GO}" build -buildvcs=false -o "${BIN_DIR}/voiceservice-amd64" ./cmd/voiceservice )

# 2. Materialize the shared, digest-pinned input facts.
log "materializing deterministic input suite in ${FIXTURE_DIR}"
"${BIN_DIR}/xarchcheck" fixtures -fixture-dir "${FIXTURE_DIR}"

declare -A MODE BINARY QEMU
MODE[arm64]="$(execution_mode arm64)"
MODE[amd64]="$(execution_mode amd64)"
BINARY[arm64]="${BIN_DIR}/voiceservice-arm64"
BINARY[amd64]="${BIN_DIR}/voiceservice-amd64"

for arch in arm64 amd64; do
  log "execution mode for ${arch}: ${MODE[${arch}]}"
done

# 3. Run every architecture that can execute; record the rest as unverified.
RAN_ARCHES=()
for arch in arm64 amd64; do
  mode="${MODE[${arch}]}"
  case "${mode}" in
    native)
      log "rendering suite natively on ${arch}"
      "${BIN_DIR}/xarchcheck" run \
        -arch "${arch}" -binary "${BINARY[${arch}]}" \
        -fixture-dir "${FIXTURE_DIR}" -work-dir "${WORK_DIR}" \
        -bundle-dir "${RESULT_DIR}/bundle-${arch}"
      RAN_ARCHES+=("${arch}")
      ;;
    qemu:*)
      emulator="${mode#qemu:}"
      log "rendering suite on ${arch} under ${emulator}"
      "${BIN_DIR}/xarchcheck" run \
        -arch "${arch}" -binary "${BINARY[${arch}]}" -qemu "${emulator}" \
        -fixture-dir "${FIXTURE_DIR}" -work-dir "${WORK_DIR}" \
        -bundle-dir "${RESULT_DIR}/bundle-${arch}"
      RAN_ARCHES+=("${arch}")
      ;;
    unavailable)
      log "WARNING: ${arch} cannot execute on this host (no native match, no qemu); marking UNVERIFIED"
      ;;
  esac
done

# 4. Conclude. Two sides -> judge. One side -> explicit unverified conclusion.
if [ "${#RAN_ARCHES[@]}" -eq 2 ]; then
  log "judging arm64 vs amd64 bundles"
  if "${BIN_DIR}/xarchcheck" judge \
      -fixture-dir "${FIXTURE_DIR}" \
      -left-arch arm64 -left-bundle "${RESULT_DIR}/bundle-arm64" \
      -right-arch amd64 -right-bundle "${RESULT_DIR}/bundle-amd64" \
      -report "${RESULT_DIR}/judge-report.json" \
      -failure-case-dir "${RESULT_DIR}/failure-case"; then
    log "PASS: arm64 and amd64 deliver compatible audio for the shared suite"
    exit 0
  fi
  log "FAIL: meaning-changing cross-architecture difference detected; failure case in ${RESULT_DIR}/failure-case"
  exit 1
fi

if [ "${#RAN_ARCHES[@]}" -eq 1 ]; then
  verified="${RAN_ARCHES[0]}"
  unverified="amd64"
  [ "${verified}" = "amd64" ] && unverified="arm64"
  conclusion="$(cat <<EOF
{
  "status": "inconclusive",
  "verified_arch": "${verified}",
  "unverified_arch": "${unverified}",
  "reason": "only one architecture could execute on this host; cross-architecture compatibility is NOT established",
  "fixture_dir": "${FIXTURE_DIR}",
  "next_step": "re-run scripts/xarch-verify.sh on a host that can execute ${unverified} (native or qemu-user), then judge the two bundles"
}
EOF
)"
  printf '%s\n' "${conclusion}" > "${RESULT_DIR}/conclusion.json"
  log "INCONCLUSIVE: ${verified} rendered successfully, but ${unverified} remains UNVERIFIED. See ${RESULT_DIR}/conclusion.json"
  exit 3
fi

die "no architecture could execute the service on this host"
