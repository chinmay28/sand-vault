#!/usr/bin/env bash
# enable-disk-health.sh — Install the SMART scanner the disk health check reads.
#
# SAND's health check can warn when the disk under a "Local folder" account is
# failing — pending sectors, a failed self-test, a drive that has tripped its
# own thresholds. The service cannot read SMART itself: it runs sandboxed with
# PrivateDevices=true, which is the right place to leave a network-facing
# process holding cloud credentials. So the reading is done by a separate
# root-run one-shot — `sand smart-scan` — that writes a small JSON report to
# the data directory, where the service picks it up.
#
# scripts/quickstart.sh installs that one-shot with the service, so an install
# made with it needs nothing from here. This script writes the same systemd
# service + hourly timer for an install made another way (scripts/deploy-linux.sh,
# a unit of your own), and runs it once so the first report exists before the
# first health check asks for it. The check is on by default in the app
# (accounts drawer → cloud health → "Disk health").
#
# Usage (run as root or with sudo):
#   sudo ./scripts/enable-disk-health.sh
#
# Take it back out:
#   sudo ./scripts/enable-disk-health.sh --remove
#
# Needs smartmontools (`smartctl`); the script says so if it is missing.

set -euo pipefail

SERVICE_NAME="${SAND_SERVICE:-sand}"
SCAN_NAME="${SERVICE_NAME}-smart"
# Seams for tests; a real install uses the defaults.
UNIT_DIR="${SAND_UNIT_DIR:-/etc/systemd/system}"
SAND_BIN="${SAND_BIN:-/opt/sand/src/sand}"
REPORT="${SAND_SMART_REPORT:-/var/lib/sand/smart.json}"

UNIT="${UNIT_DIR}/${SCAN_NAME}.service"
TIMER="${UNIT_DIR}/${SCAN_NAME}.timer"

if [[ "${1:-}" == "--remove" ]]; then
  systemctl disable --now "${SCAN_NAME}.timer" 2>/dev/null || true
  rm -f "$UNIT" "$TIMER" "$REPORT"
  systemctl daemon-reload
  echo "Removed ${SCAN_NAME}.service, ${SCAN_NAME}.timer and ${REPORT}."
  echo "Switch the disk check off in the app too, or it will say the report is missing."
  exit 0
fi

if [[ $EUID -ne 0 ]]; then
  echo "Run this as root (sudo): reading SMART is a raw device read." >&2
  exit 1
fi

if ! command -v smartctl >/dev/null; then
  echo "smartctl is not installed. On Debian/Ubuntu/Raspberry Pi OS:" >&2
  echo "  sudo apt install smartmontools" >&2
  exit 1
fi

if [[ ! -x "$SAND_BIN" ]]; then
  echo "No sand binary at ${SAND_BIN} — set SAND_BIN to where it lives." >&2
  exit 1
fi

cat > "$UNIT" <<EOF
# Written by scripts/enable-disk-health.sh. Reads every disk's SMART health
# and writes ${REPORT} for the ${SERVICE_NAME} service's health check.
[Unit]
Description=SAND disk SMART scan

[Service]
Type=oneshot
ExecStart=${SAND_BIN} smart-scan --out ${REPORT}
EOF

cat > "$TIMER" <<EOF
# Written by scripts/enable-disk-health.sh.
[Unit]
Description=SAND disk SMART scan, hourly

[Timer]
# Soon after boot, so a machine that is only ever up for a while still gets a
# report; then hourly, which is the freshness the service's staleness window
# (internal/provider/disksmart.go) is sized against.
OnBootSec=5min
OnUnitActiveSec=1h
RandomizedDelaySec=2min

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now "${SCAN_NAME}.timer"

# The first report, now, so the first health check after switching the toggle
# on finds one rather than warning that the scanner has never run.
systemctl start "${SCAN_NAME}.service"

echo
echo "Installed. The scan runs hourly; the latest report is ${REPORT}."
echo "The disk check is on by default in the app: accounts drawer → cloud health → Disk health."
