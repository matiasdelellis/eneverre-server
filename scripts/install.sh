#!/usr/bin/env bash
# install.sh - download, install, update or remove eneverre from GitHub releases.
#
# Usage:
#   install.sh                          install the latest release
#   install.sh --version v1.0.0         install a specific release
#   install.sh --list                   show the last few releases
#   install.sh --target-dir /opt/bin    install somewhere else
#   install.sh --install-service        also install + start the systemd unit
#   install.sh --force                  overwrite an existing systemd unit
#   install.sh --no-verify              skip the SHA256 check
#   install.sh --dry-run                show what would happen, do nothing
#   install.sh --uninstall              stop the service, remove the unit and the binary
#   install.sh --uninstall --yes        ...without the confirmation prompt
#   install.sh --uninstall --dry-run    preview an uninstall without removing
#
# A bare version number is accepted too: --version 1.0.0 is treated as v1.0.0.
#
# Can be run straight from a URL:
#   curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash
#   sudo bash -c "$(curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh)"
#
# Requires: bash, curl, tar. sha256sum (or shasum on macOS) is required for
# the default verification - use --no-verify to skip.

set -euo pipefail

# ---- Defaults --------------------------------------------------------------
REPO="matiasdelellis/eneverre-server"
BINARY="eneverre"
TARGET_DIR="/usr/local/bin"
VERSION=""
LIST=false
VERIFY=true
DRY_RUN=false
INSTALL_SERVICE=false
FORCE=false
UNINSTALL=false
ASSUME_YES=false

# ---- Help ------------------------------------------------------------------
usage() {
  cat <<EOF
install.sh - download, install, update or remove eneverre from GitHub releases.

Usage:
  install.sh                          install the latest release
  install.sh --version v1.0.0         install a specific release
  install.sh --list                   show the last few releases
  install.sh --target-dir /opt/bin    install somewhere else
  install.sh --install-service        also install + start the systemd unit
  install.sh --force                  overwrite an existing systemd unit
  install.sh --no-verify              skip the SHA256 check
  install.sh --dry-run                show what would happen, do nothing
  install.sh --uninstall              stop the service, remove the unit and the binary
  install.sh --uninstall --yes        ...without the confirmation prompt
  install.sh --uninstall --dry-run    preview an uninstall without removing

A bare version number is accepted too: --version 1.0.0 is treated as v1.0.0.

Can be run straight from a URL:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | sudo bash

Requires: bash, curl, tar. sha256sum (or shasum on macOS) is required for
the default verification - use --no-verify to skip.

Note: --install-service only works on Linux. The unit file is copied to
/etc/systemd/system/eneverre.service, but the existing file is preserved
unless --force is also given. The user's eneverre.ini and cameras.d/ are
NEVER touched - manage those with the recipes in doc/example/README.md.

Note: --uninstall removes the binary, the unit file, and stops the service.
It does NOT remove /etc/eneverre/ (config) or /var/lib/eneverre/ (state) -
delete those manually if you want a fully clean uninstall.
EOF
}

# ---- Argument parsing ------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)         VERSION="$2"; shift 2 ;;
    --target-dir)      TARGET_DIR="$2"; shift 2 ;;
    --list)            LIST=true; shift ;;
    --install-service) INSTALL_SERVICE=true; shift ;;
    --force)           FORCE=true; shift ;;
    --no-verify)       VERIFY=false; shift ;;
    --dry-run)         DRY_RUN=true; shift ;;
    --uninstall)       UNINSTALL=true; shift ;;
    -y|--yes)          ASSUME_YES=true; shift ;;
    -h|--help)         usage; exit 0 ;;
    *)                 echo "error: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

# ---- Helpers ---------------------------------------------------------------
die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }
# run executes a command, or just prints it when --dry-run is set.
run() { if $DRY_RUN; then echo "  [dry-run] $*"; else "$@"; fi; }

# --uninstall is mutually exclusive with everything that does an install
# (--dry-run is allowed, to preview what would be removed).
if $UNINSTALL; then
  if [[ -n "$VERSION" ]] || $LIST || $INSTALL_SERVICE || $FORCE || ! $VERIFY; then
    die "--uninstall cannot be combined with other install flags"
  fi
fi

for tool in curl tar; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# ---- Detect platform -------------------------------------------------------
case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *)      die "unsupported OS: $(uname -s) (Windows is not supported by this script)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l) ARCH=arm ;;
  *)             die "unsupported architecture: $(uname -m)" ;;
esac

log "platform: ${OS}/${ARCH}"

# ---- GitHub API ------------------------------------------------------------
api() { curl -fsSL -H 'Accept: application/vnd.github+json' "$1"; }

if $LIST; then
  log "recent releases of ${REPO}:"
  api "https://api.github.com/repos/${REPO}/releases?per_page=10" \
    | grep -E '"tag_name"' \
    | sed -E 's/.*"tag_name": *"([^"]+)".*/  \1/'
  exit 0
fi

# ---- Uninstall -------------------------------------------------------------
if $UNINSTALL; then
  [[ "$OS" == "linux" || -x "${TARGET_DIR}/${BINARY}" ]] || die "no install found at ${TARGET_DIR}/${BINARY}"

  if ! $ASSUME_YES && ! $DRY_RUN; then
    printf "Remove %s and the systemd unit? [y/N] " "${TARGET_DIR}/${BINARY}" >&2
    if ! read -r REPLY; then
      echo "" >&2
      die "refusing to uninstall non-interactively without --yes"
    fi
    [[ $REPLY =~ ^[Yy]$ ]] || { echo "aborted" >&2; exit 0; }
  fi

  if [[ "$OS" == "linux" ]] && command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet eneverre 2>/dev/null; then
      log "stopping eneverre";  run systemctl stop eneverre
    fi
    if systemctl is-enabled --quiet eneverre 2>/dev/null; then
      log "disabling eneverre"; run systemctl disable eneverre
    fi
    if [[ -f /etc/systemd/system/eneverre.service ]]; then
      log "removing /etc/systemd/system/eneverre.service"
      run rm -f /etc/systemd/system/eneverre.service
      run systemctl daemon-reload
    fi
  fi

  if [[ -x "${TARGET_DIR}/${BINARY}" ]]; then
    log "removing ${TARGET_DIR}/${BINARY}"
    run rm -f "${TARGET_DIR}/${BINARY}"
  else
    log "${TARGET_DIR}/${BINARY} not present (skipping)"
  fi

  log "uninstall complete"
  log "left untouched: /etc/eneverre/ (config) and /var/lib/eneverre/ (state) - remove manually if you want a fully clean uninstall"
  exit 0
fi

# Accept a bare version number (1.0.0) as well as a tag (v1.0.0). Releases
# are tagged with a leading 'v', so add it if the user left it off.
if [[ -n "$VERSION" && "$VERSION" =~ ^[0-9] ]]; then
  VERSION="v${VERSION}"
fi

if [[ -z "$VERSION" ]]; then
  log "fetching latest release tag from GitHub"
  VERSION="$(api "https://api.github.com/repos/${REPO}/releases/latest" \
            | sed -nE 's/.*"tag_name": *"([^"]+)".*/\1/p' | head -n1)"
  [[ -n "$VERSION" ]] || die "could not determine the latest release (GitHub API rate-limited?)"
fi
log "version: ${VERSION}"

# ---- Preflight: permissions ------------------------------------------------
# Fail early with an actionable message instead of a cryptic error halfway
# through. Skipped for --dry-run, which writes nothing.
if ! $DRY_RUN; then
  if [[ -d "$TARGET_DIR" ]]; then
    [[ -w "$TARGET_DIR" ]] || die "no write permission for ${TARGET_DIR} - re-run with sudo"
  else
    [[ -w "$(dirname "$TARGET_DIR")" ]] || die "cannot create ${TARGET_DIR} - re-run with sudo"
  fi
  if $INSTALL_SERVICE && [[ "$(id -u)" -ne 0 ]]; then
    die "--install-service needs root - re-run with sudo"
  fi
fi

# ---- Download + verify -----------------------------------------------------
TARBALL="${BINARY}-${VERSION}-${OS}-${ARCH}.tar.gz"
CHECKSUM="${TARBALL}.sha256"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "downloading ${TARBALL}"
curl -fsSL -o "${TMP}/${TARBALL}" "${BASE_URL}/${TARBALL}" \
  || die "could not download ${TARBALL} - does ${VERSION} ship an ${OS}/${ARCH} build? (try: install.sh --list)"

if $VERIFY; then
  log "verifying SHA256"
  curl -fsSL -o "${TMP}/${CHECKSUM}" "${BASE_URL}/${CHECKSUM}" \
    || die "could not download the checksum ${CHECKSUM} (use --no-verify to skip)"
  EXPECTED="$(awk '{print $1}' "${TMP}/${CHECKSUM}")"
  ACTUAL="$(sha256_file "${TMP}/${TARBALL}")"
  [[ "$EXPECTED" == "$ACTUAL" ]] || die "SHA256 mismatch (expected ${EXPECTED}, got ${ACTUAL})"
fi

# ---- Extract + install -----------------------------------------------------
log "extracting"
tar -xzf "${TMP}/${TARBALL}" -C "$TMP"

# The tarball wraps everything in eneverre-<ver>-<os>-<arch>/. Find the
# binary inside that wrapper (works regardless of the wrapper name). Match
# by name only: BSD find (macOS) has no -executable predicate.
BIN_PATH="$(find "$TMP" -mindepth 2 -maxdepth 2 -type f -name "$BINARY" | head -n1)"
[[ -n "$BIN_PATH" && -f "$BIN_PATH" ]] || die "expected binary ${BINARY} not found in tarball"

TARGET="${TARGET_DIR}/${BINARY}"
if [[ -x "$TARGET" ]]; then
  CURRENT="$("$TARGET" --version 2>/dev/null | awk '{print $NF}' || true)"
  log "replacing existing install (current: ${CURRENT:-unknown})"
else
  log "installing new binary"
fi

if $DRY_RUN; then
  log "[dry-run] atomically install ${BIN_PATH} -> ${TARGET}"
  log "[dry-run] ${TARGET} --version"
  log "done (dry-run; nothing written to ${TARGET_DIR})"
else
  [[ -d "$TARGET_DIR" ]] || install -d "$TARGET_DIR"
  # Stage the new binary next to the target, then rename over it.
  # rename(2) is atomic and - unlike writing in place - succeeds even
  # when the current binary is running: an in-place overwrite of a live
  # binary fails with ETXTBSY, which would break updating a running
  # service. The old inode stays alive for the running process until it
  # exits.
  STAGED="$(mktemp "${TARGET}.XXXXXX")" \
    || die "cannot write to ${TARGET_DIR} - re-run with sudo"
  if ! install -m 0755 "$BIN_PATH" "$STAGED"; then
    rm -f "$STAGED"
    die "failed to stage the new binary in ${TARGET_DIR}"
  fi
  mv -f "$STAGED" "$TARGET"
  log "verifying"
  "$TARGET" --version
  log "done"
fi

# ---- Hint: a running service keeps the old binary until restarted -----------
if ! $DRY_RUN && ! $INSTALL_SERVICE && [[ "$OS" == "linux" ]] \
   && command -v systemctl >/dev/null 2>&1 \
   && systemctl is-active --quiet eneverre 2>/dev/null; then
  log "note: the eneverre service is still running the previous binary"
  log "      apply the update with: sudo systemctl restart eneverre"
fi

# ---- Optional: systemd unit ------------------------------------------------
if $INSTALL_SERVICE; then
  [[ "$OS" == "linux" ]] || die "--install-service is only supported on Linux (this is ${OS})"

  UNIT_SRC="$(dirname "$BIN_PATH")/doc/example/eneverre.service"
  UNIT_DEST="/etc/systemd/system/eneverre.service"

  if [[ ! -f "$UNIT_SRC" ]]; then
    die "unit file not found in tarball: ${UNIT_SRC}"
  fi

  if [[ -e "$UNIT_DEST" ]] && ! $FORCE; then
    log "service file already at ${UNIT_DEST} (pass --force to overwrite)"
  else
    log "installing unit to ${UNIT_DEST}"
    run install -m 0644 "$UNIT_SRC" "$UNIT_DEST"
  fi

  log "systemctl daemon-reload";      run systemctl daemon-reload
  log "systemctl enable eneverre";    run systemctl enable eneverre

  if ! $DRY_RUN && systemctl is-active --quiet eneverre; then
    log "systemctl restart eneverre"; run systemctl restart eneverre
  else
    log "systemctl start eneverre";   run systemctl start eneverre
  fi

  if $DRY_RUN; then
    log "service status: (skipped in dry-run)"
  else
    log "service status: $(systemctl is-active eneverre || true)"
  fi
fi
