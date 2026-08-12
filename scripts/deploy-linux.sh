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
#   bind    = 127.0.0.1   (see the note at the end before changing this)
#
# After running:
#   systemctl status sand
#   journalctl -u sand -f

set -euo pipefail

BINARY="${1:-}"
PORT="${2:-8123}"
BIND="${3:-127.0.0.1}"
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
    echo "    NOTE: bound to localhost only. SAND Vault takes your vault password"
    echo "    this connection and sends decrypted files back over it, so put TLS"
    echo "    in front before exposing it:"
    echo "      - place a reverse proxy (nginx/caddy) in front, then"
    echo "      - re-run with bind=0.0.0.0"
fi
