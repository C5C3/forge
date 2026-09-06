#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/install-test-deps.sh — Install pinned E2E test dependencies.
#
# By default installs: chainsaw, kind, kubectl. The Flux CLI is optional after
# the FluxInstance bootstrap migration and is installed
# only when WITH_FLUX_CLI=true is exported.

set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

# ---------------------------------------------------------------------------
# Pinned versions
# ---------------------------------------------------------------------------
CHAINSAW_VERSION="v0.2.15"
FLUX_VERSION="2.9.5"
KIND_VERSION="v0.32.0"
KUBECTL_VERSION="v1.37.0"

# ---------------------------------------------------------------------------
# Pinned SHA256 hashes.
#
# Supply-chain hardening: hashes are pinned as constants so that a compromised
# GitHub Release page cannot substitute both the binary and its checksum file
# simultaneously.  To update after a version bump, download the new release
# artifacts, compute sha256sum, and replace the values below.
#
# Chainsaw v0.2.14 hashes are fetched from upstream (not pinned) because pinned
# values were not available at authoring time. Pin them here once verified.
# ---------------------------------------------------------------------------
# Use plain variables instead of associative arrays for bash 3.2 (macOS) compatibility.
# These are referenced via indirect expansion (e.g. ${!_flux_var}).
# shellcheck disable=SC2034
FLUX_SHA256_linux_amd64="c2c397a52930f52d2005c01d276116b059d062de379386d58e98115380a766a2"
# shellcheck disable=SC2034
FLUX_SHA256_linux_arm64="dcab945f8d8658662fa1db93e150a602ba1f91da367a82e099bd65595fc7c62c"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_amd64="162e11dd0fd44d1d87f3b8308b74e8ef8fedd0a550b8501f0365f329efc476f8"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_arm64="5350675411f1c8c20fb7fd827450933e939f7ab82a2cd92f8fa2abe497761911"

# shellcheck disable=SC2034
KIND_SHA256_linux_amd64="50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54"
# shellcheck disable=SC2034
KIND_SHA256_linux_arm64="b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d"
# shellcheck disable=SC2034
KIND_SHA256_darwin_amd64="295ac6d0d634c9819c9907df45e3017d1f13166bd13c3404c45e79f7faa47498"
# shellcheck disable=SC2034
KIND_SHA256_darwin_arm64="dca67911095a110c2b5c36e26df6cac860c602033e456c0db47be498cdef1ebb"

# shellcheck disable=SC2034
KUBECTL_SHA256_linux_amd64="8b8f088da2dab964f853b38464033b1be15ede2839eca751482357c45abdd05a"
# shellcheck disable=SC2034
KUBECTL_SHA256_linux_arm64="0ecf44450ee6063bf19dd166a103ee6df4a9034455c2abce626e6eea657d73fb"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_amd64="71a3aa7c2ee2c974d9fbb462cba0c5c04a4df2e8d85eee94714fd819ea3c4e63"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_arm64="c9e4f713d6fee0043a3d835cca13077cda2bc0973840eb9779360df0b5bdfc69"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# detect_platform — Set OS and ARCH from uname.
# ---------------------------------------------------------------------------
detect_platform() {
  local uname_os uname_arch
  uname_os="$(uname -s)"
  uname_arch="$(uname -m)"

  case "${uname_os}" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)
      log "ERROR: Unsupported OS: ${uname_os}"
      exit 1
      ;;
  esac

  case "${uname_arch}" in
    x86_64)  ARCH="amd64" ;;
    aarch64 | arm64) ARCH="arm64" ;;
    *)
      log "ERROR: Unsupported architecture: ${uname_arch}"
      exit 1
      ;;
  esac

  log "Detected platform: ${OS}/${ARCH}"
}

# ---------------------------------------------------------------------------
# verify_sha256 — Verify SHA256 checksum of a downloaded file.
#
# Arguments:
#   $1 — path to the file to verify
#   $2 — expected SHA256 hex digest
# ---------------------------------------------------------------------------
verify_sha256() {
  local file="$1"
  local expected="$2"
  if [[ -z "${expected}" ]]; then
    log "ERROR: Empty expected SHA256 hash for $(basename "${file}")."
    exit 1
  fi
  local actual
  if command -v sha256sum &>/dev/null; then
    actual=$(sha256sum "${file}" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "${file}" | awk '{print $1}')
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    log "ERROR: SHA256 checksum mismatch for $(basename "${file}")"
    log "  expected: ${expected}"
    log "  actual:   ${actual}"
    exit 1
  fi
  log "  SHA256 checksum verified."
}

# ---------------------------------------------------------------------------
# install_chainsaw — Install Kyverno Chainsaw (tarball).
# ---------------------------------------------------------------------------
install_chainsaw() {
  local target="${INSTALL_DIR}/chainsaw"
  local want="${CHAINSAW_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "chainsaw ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing chainsaw ${want}..."
  local url="https://github.com/kyverno/chainsaw/releases/download/${want}/chainsaw_${OS}_${ARCH}.tar.gz"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/chainsaw.tar.gz"

  # Verify download integrity against release checksums.
  # NOTE: Chainsaw checksums are fetched from upstream (not pinned) because the
  # release asset naming changed across versions and pinned hashes were not
  # available at authoring time. Pin them in the CHAINSAW_SHA256 array once verified.
  local checksums_url="https://github.com/kyverno/chainsaw/releases/download/${want}/checksums.txt"
  curl -fsSL "${checksums_url}" -o "${tmpdir}/checksums.txt"
  local expected_hash
  expected_hash=$(awk "/chainsaw_${OS}_${ARCH}\\.tar\\.gz\$/ {print \$1}" "${tmpdir}/checksums.txt")
  verify_sha256 "${tmpdir}/chainsaw.tar.gz" "${expected_hash}"

  tar -xzf "${tmpdir}/chainsaw.tar.gz" -C "${tmpdir}" chainsaw
  install -m 0755 "${tmpdir}/chainsaw" "${target}"
  log "chainsaw ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_flux — Install Flux CLI (tarball).
# ---------------------------------------------------------------------------
install_flux() {
  local target="${INSTALL_DIR}/flux"
  local want="${FLUX_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version --client 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "flux ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing flux ${want}..."
  local url="https://github.com/fluxcd/flux2/releases/download/v${want}/flux_${want}_${OS}_${ARCH}.tar.gz"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/flux.tar.gz"

  # Verify download integrity against pinned SHA256 hash.
  local _flux_var="FLUX_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_flux_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for flux ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/flux.tar.gz" "${expected_hash}"

  tar -xzf "${tmpdir}/flux.tar.gz" -C "${tmpdir}" flux
  install -m 0755 "${tmpdir}/flux" "${target}"
  log "flux ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_kind — Install kind (standalone binary).
# ---------------------------------------------------------------------------
install_kind() {
  local target="${INSTALL_DIR}/kind"
  local want="${KIND_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "kind ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing kind ${want}..."
  local url="https://github.com/kubernetes-sigs/kind/releases/download/${want}/kind-${OS}-${ARCH}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/kind"

  # Verify download integrity against pinned SHA256 hash.
  local _kind_var="KIND_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_kind_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for kind ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/kind" "${expected_hash}"

  install -m 0755 "${tmpdir}/kind" "${target}"
  log "kind ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_kubectl — Install kubectl (standalone binary).
# ---------------------------------------------------------------------------
install_kubectl() {
  local target="${INSTALL_DIR}/kubectl"
  local want="${KUBECTL_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version --client 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "kubectl ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing kubectl ${want}..."
  local url="https://dl.k8s.io/release/${want}/bin/${OS}/${ARCH}/kubectl"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/kubectl"

  # Verify download integrity against pinned SHA256 hash.
  local _kubectl_var="KUBECTL_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_kubectl_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for kubectl ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/kubectl" "${expected_hash}"

  install -m 0755 "${tmpdir}/kubectl" "${target}"
  log "kubectl ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  log "=== Installing E2E Test Dependencies ==="
  detect_platform
  mkdir -p "${INSTALL_DIR}"
  install_chainsaw
  # Flux CLI is optional: the kind Quick Start bootstraps Flux via the
  # flux-operator FluxInstance and no longer shells out to `flux`
  # Set WITH_FLUX_CLI=true to install it anyway.
  if [[ "${WITH_FLUX_CLI:-false}" == "true" ]]; then
    install_flux
  fi
  install_kind
  install_kubectl
  log "=== Done ==="
}

main "$@"
