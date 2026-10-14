#!/usr/bin/env bash
# Installs the fedarisha server on a Debian/Ubuntu host.
#
# What it does: creates a service user, installs a versioned binary, drops a
# hardened systemd unit, and waits for a config to appear. It never writes a
# config — the storage credentials must be provisioned separately (see
# deploy/README.md), so a re-run cannot clobber live credentials.

set -euo pipefail

REPO="${REPO:-https://github.com/s3network/s3-bypass-protocol}"
VERSION="${VERSION:?set VERSION to a release tag, e.g. VERSION=v0.1.0-fork.1}"
SHA256="${SHA256:?set SHA256 to the published digest of the release archive; this script will not install an unverified binary}"
INSTALL_DIR="${INSTALL_DIR:-/opt/s3bypass-protocol}"
CONFIG_DIR="${CONFIG_DIR:-/etc/s3bypass-protocol}"
STATE_DIR="${STATE_DIR:-/var/lib/s3bypass-protocol}"
SERVICE_USER="${SERVICE_USER:-s3bypass}"
BINARY_URL="${REPO}/releases/download/${VERSION}/Xray-linux-64.zip"
BINARY_PATH="${INSTALL_DIR}/xray"

log() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root"
command -v curl >/dev/null || die "curl is required"
command -v unzip >/dev/null || die "unzip is required"

log "checking the release exists"
curl -fsSLI "${BINARY_URL}" >/dev/null || die "cannot fetch ${BINARY_URL}"

log "creating the service user"
if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
  useradd --system --home-dir "${STATE_DIR}" --shell /usr/sbin/nologin "${SERVICE_USER}"
fi
install -d -m 0750 -o root -g "${SERVICE_USER}" "${INSTALL_DIR}" "${STATE_DIR}"
install -d -m 0750 -o root -g "${SERVICE_USER}" "${CONFIG_DIR}"

log "installing ${VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
curl -fsSL "${BINARY_URL}" -o "${tmp}/core.zip"
# Verify the published digest before running anything from it.
#
# Mandatory, not optional. It used to warn and continue, so the common case —
# an operator who forgets to export SHA256 — installed and ran an unverified
# binary as root and was told, in a line that scrolls past, that nothing had
# been checked. The upgrade path has always refused to do this
# (upgrade-server.sh: "refusing an unverifiable binary"); this script has to
# agree with it.
echo "${SHA256}  ${tmp}/core.zip" | sha256sum -c - >/dev/null \
  || die "SHA-256 mismatch for the downloaded archive"
log "digest verified"
unzip -q "${tmp}/core.zip" -d "${tmp}/unpacked"
# -print -quit rather than `| head -1`: under pipefail, head closing the pipe
# early can hand find a SIGPIPE and fail a search that actually succeeded, which
# here means aborting an install that was about to work.
found="$(find "${tmp}/unpacked" -type f -name xray -print -quit)"
[ -n "${found}" ] || die "no xray binary inside the archive"
install -m 0755 -o root -g root "${found}" "${BINARY_PATH}"

log "installing the systemd unit"
install -m 0644 -o root -g root "$(dirname "$0")/s3bypass-protocol.service" \
  /etc/systemd/system/s3bypass-protocol.service
systemctl daemon-reload

if [ -r "${CONFIG_DIR}/server.json" ]; then
  log "config present — enabling and starting"
  systemctl enable --now s3bypass-protocol
  sleep 2
  systemctl --no-pager --lines=20 status s3bypass-protocol || true
else
  log "no config at ${CONFIG_DIR}/server.json — unit installed but NOT started"
  cat <<EOF

Next: provision the config, then start the service.

  ${CONFIG_DIR}/server.json   root:${SERVICE_USER} 0640

It needs: log, inbounds[0] { protocol: fedarisha, listen, port,
settings: { storage: { type, bucket, endpoint, region, accessKey,
secretKey, prefix }, clients: [{ id, level }] } }.

build-server-config.mjs in the repo can generate it from an existing client
config without printing any values.

  systemctl enable --now s3bypass-protocol
EOF
fi

log "done"