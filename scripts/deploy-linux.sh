#!/usr/bin/env bash
# deploy-linux.sh — Install an ALREADY-BUILT SAND Vault binary as a systemd service.
#
# For a one-command install that also fetches, builds, upgrades and rolls back,
# use scripts/quickstart.sh instead. This script is the small path: you have a
# binary, you want it running. Both write the same unit and use the same data
# directory, so they can be used interchangeably on the same host.
#
# Usage (run as root or with sudo):
#   sudo ./scripts/deploy-linux.sh [binary] [port] [bind]
#
# Defaults:
#   binary  = ./sand  (or sand.exe will be tried)
#   port    = 8123
#   bind    = 0.0.0.0     (all interfaces — see the note at the end)
#
# The unit is sandboxed with ProtectSystem=strict, so a "Local folder" account
# on an external disk is read-only to the service until its directory is
# granted. Do it here with SAND_LOCAL_PATHS (colon-separated), or later with
# scripts/allow-local-path.sh:
#
#   sudo SAND_LOCAL_PATHS=/media/you/Disk/SANDVault ./scripts/deploy-linux.sh
#
# After running:
#   systemctl status sand
#   journalctl -u sand -f

set -euo pipefail

BINARY="${1:-}"
PORT="${2:-8123}"
BIND="${3:-0.0.0.0}"
# The vault holds cloud credentials and the map of every stored file, so it
# lives in a stable directory the service can actually write to. The service
# user has no home and the unit sets ProtectHome, so the default
# ~/.sand/vault.sand would be unreachable — this must be passed explicitly.
DATA_DIR="${SAND_DATA_DIR:-/var/lib/sand}"
VAULT_PATH="${DATA_DIR}/vault.sand"

# ── Locate binary ────────────────────────────────────────────────────────────
if [[ -z "${BINARY}" ]]; then
    for candidate in ./sand ./sand.exe dist/sand-*-linux-amd64; do
        if [[ -f "${candidate}" ]]; then
            BINARY="${candidate}"
            break
        fi
    done
fi

if [[ -z "${BINARY}" || ! -f "${BINARY}" ]]; then
    echo "ERROR: sand binary not found. Build it first:"
    echo "  make build"
    echo "  # or for a specific target:"
    echo "  ./scripts/build-release.sh"
    exit 1
fi

echo "==> Deploying SAND Vault"
echo "    binary : ${BINARY}"
echo "    port   : ${PORT}"
echo "    bind   : ${BIND}"
echo "    vault  : ${VAULT_PATH}"

# ── Install binary ───────────────────────────────────────────────────────────
install -m 755 "${BINARY}" /usr/local/bin/sand
echo "    installed → /usr/local/bin/sand"

# ── Create service user ──────────────────────────────────────────────────────
if ! id -u sand &>/dev/null; then
    useradd --system --no-create-home --shell /usr/sbin/nologin sand
    echo "    created system user 'sand'"
fi

# ── Data directory ───────────────────────────────────────────────────────────
# Created before the unit so the first start has somewhere to put the vault.
install -d -o sand -g sand -m 750 "${DATA_DIR}" "${DATA_DIR}/backups"
echo "    data dir → ${DATA_DIR}"

# ── Write systemd unit ───────────────────────────────────────────────────────
cat > /etc/systemd/system/sand.service <<EOF
[Unit]
Description=SAND Vault — Secure Archival Network Distribution
After=network.target

[Service]
Type=simple
User=sand
Group=sand
ExecStart=/usr/local/bin/sand serve --port ${PORT} --bind ${BIND} --vault ${VAULT_PATH}
Environment=SAND_VAULT=${VAULT_PATH}
Restart=on-failure
RestartSec=5s
# Security hardening. The service gets write access to its data directory and
# nothing else; ProtectHome is why --vault above is not optional.
NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF

echo "    wrote /etc/systemd/system/sand.service"

# ── Local folder paths ───────────────────────────────────────────────────────
# ProtectSystem=strict above makes every path outside DATA_DIR read-only to the
# service, so a "Local folder" account on an external disk fails to connect
# with "read-only file system" until its directory is listed too. Grant paths
# here at install time, or later with scripts/allow-local-path.sh — both write
# this same drop-in, which survives re-runs of either installer.
DROPIN_DIR="/etc/systemd/system/sand.service.d"
if [[ -n "${SAND_LOCAL_PATHS:-}" ]]; then
    mkdir -p "${DROPIN_DIR}"
    {
        echo "# Paths a Local folder account may use. Also managed by"
        echo "# scripts/allow-local-path.sh. A leading '-' lets the service start"
        echo "# when the drive is not plugged in."
        echo "[Service]"
        # Split on ':' the way PATH is split, so drives with spaces still work.
        IFS=':' read -r -a sand_local_paths <<< "${SAND_LOCAL_PATHS}"
        for entry in "${sand_local_paths[@]}"; do
            [[ -n "${entry}" ]] && printf 'ReadWritePaths=-"%s"\n' "${entry%/}"
        done
    } > "${DROPIN_DIR}/10-local-paths.conf"
    echo "    local paths → ${DROPIN_DIR}/10-local-paths.conf"
fi

# ── Enable and start ─────────────────────────────────────────────────────────
systemctl daemon-reload
systemctl enable sand
systemctl restart sand

echo ""
echo "==> SAND Vault is running!"
echo "    status : systemctl status sand"
echo "    logs   : journalctl -u sand -f"
echo "    url    : http://${BIND}:${PORT}"
echo "    vault  : ${VAULT_PATH}"
echo ""
echo "    First run? Create the vault and connect accounts:"
echo "      sudo -u sand /usr/local/bin/sand --vault ${VAULT_PATH} vault init"
echo "      sudo -u sand /usr/local/bin/sand --vault ${VAULT_PATH} remote kinds"
echo ""
echo "    BACK UP ${VAULT_PATH} — it is the only record of which account holds"
echo "    which part of which file, and the only copy of your cloud credentials."
if [[ "${BIND}" == "127.0.0.1" ]]; then
    echo ""
    echo "    NOTE: bound to localhost only — reachable from this machine alone."
    echo "    Re-run with bind=0.0.0.0 to expose it, ideally behind TLS."
else
    echo ""
    echo "    WARNING: bound to ${BIND} — reachable from your whole network."
    echo "    SAND Vault takes your vault password over this connection and sends"
    echo "    decrypted files back over it, and /api/vault/unlock answers anyone"
    echo "    who can reach the port. On plain HTTP all of that is in the clear."
    echo "    Put TLS in front of it: see scripts/nginx-sand.conf, or Tailscale"
    echo "    Serve. Re-run with bind=127.0.0.1 to close it again."
fi
