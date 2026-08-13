#!/usr/bin/env bash
# allow-local-path.sh — Let the SAND service write to a local folder.
#
# The systemd unit both installers write is hardened with ProtectSystem=strict,
# which makes the whole filesystem read-only to the service apart from the
# paths its ReadWritePaths= lines name: the data directory, and the mount roots
# a removable disk or a network share normally appears under (/media,
# /run/media, /mnt, /srv). A "Local folder" account under one of those needs
# nothing from this script.
#
# A vault folder somewhere else — /data/SANDVault, say — is read-only to the
# service, and connecting it fails with:
#
#   could not connect to Local folder: /data/SANDVault is not writable:
#   read-only file system
#
# The drive is fine; the service simply cannot see it as writable. This script
# adds the paths you name to ReadWritePaths= via a drop-in, so upgrades and
# re-runs of quickstart.sh (which rewrite the main unit, never the drop-in)
# keep them.
#
# Usage (run as root or with sudo):
#   sudo ./scripts/allow-local-path.sh /data/SANDVault [more paths...]
#
# List what is currently granted, by the unit and by the drop-in:
#   sudo ./scripts/allow-local-path.sh --list
#
# Take a path back out:
#   sudo ./scripts/allow-local-path.sh --remove /data/SANDVault

set -euo pipefail

SERVICE_NAME="${SAND_SERVICE:-sand}"
SVC_USER="${SAND_USER:-sand}"
# SAND_UNIT_DIR is a seam for tests; in a real install this is the only place
# systemd reads units from.
UNIT_DIR="${SAND_UNIT_DIR:-/etc/systemd/system}"
UNIT="${UNIT_DIR}/${SERVICE_NAME}.service"
DROPIN_DIR="${UNIT_DIR}/${SERVICE_NAME}.service.d"
DROPIN="${DROPIN_DIR}/10-local-paths.conf"

C_OFF=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_DIM=''
if [ -t 1 ]; then
    C_OFF=$'\033[0m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
    C_YELLOW=$'\033[33m'; C_DIM=$'\033[2m'
fi
ok()   { printf '%sok  %s %s\n' "$C_GREEN" "$C_OFF" "$*"; }
warn() { printf '%swarn%s %s\n' "$C_YELLOW" "$C_OFF" "$*" >&2; }
die()  { printf '%serr %s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }

usage() {
    awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
    exit "${1:-0}"
}

# Paths a unit file grants, one per line, stripped of the systemd syntax around
# them: the leading "-" that says a missing path must not fail the unit, and
# the quotes that keep a path with a space in it from being read as two.
granted_paths() {
    [ -f "$1" ] || return 0
    grep -E '^ReadWritePaths=' "$1" 2>/dev/null |
        sed -e 's/^ReadWritePaths=//' -e 's/^-//' -e 's/^"//' -e 's/"$//' |
        grep -v '^$' || true
}

# What the drop-in grants — the paths this script owns and rewrites.
current_paths() { granted_paths "$DROPIN"; }

# What the main unit grants on its own: the data directory and the mount roots
# the installers write. Read from the file rather than assumed, so a unit
# written by an older installer (data directory only) is described honestly.
unit_paths() { granted_paths "$UNIT"; }

COVERING_ROOT=''

# covered_by_unit reports whether the main unit already makes $1 writable, by
# naming it or by granting a directory above it — the usual case for a drive
# under /media. Saves a drop-in entry that would grant nothing new.
covered_by_unit() {
    local path="$1" granted
    while IFS= read -r granted; do
        [ -n "$granted" ] || continue
        case "$path" in
            "$granted"|"$granted"/*) COVERING_ROOT="$granted"; return 0 ;;
        esac
    done < <(unit_paths)
    return 1
}

# writable_by_service tests plain filesystem permission for the service user —
# the check the systemd sandbox knows nothing about. runuser ships with
# util-linux; sudo is the fallback for hosts that lack it.
writable_by_service() {
    if command -v runuser >/dev/null 2>&1; then
        runuser -u "$SVC_USER" -- test -w "$1" 2>/dev/null
    elif command -v sudo >/dev/null 2>&1; then
        sudo -u "$SVC_USER" test -w "$1" 2>/dev/null
    else
        return 0 # nothing to test with; say nothing rather than cry wolf
    fi
}

MODE="add"
case "${1:-}" in
    -h|--help)   usage 0 ;;
    --list)      MODE="list" ;;
    --remove|-r) MODE="remove"; shift ;;
    '')          usage 1 ;;
esac

if [ "$MODE" = "list" ]; then
    if [ -f "$UNIT" ]; then
        echo "granted by ${UNIT}:"
        unit_paths | sed 's/^/  /'
    else
        echo "no unit at ${UNIT} — is the service installed?"
    fi
    if [ -f "$DROPIN" ]; then
        echo "granted by ${DROPIN}:"
        current_paths | sed 's/^/  /'
    else
        echo "no drop-in at ${DROPIN} — nothing granted beyond the unit above"
    fi
    exit 0
fi

[ "$(id -u)" -eq 0 ] || die "Run as root: sudo $0 $*"
command -v systemctl >/dev/null 2>&1 || die "systemd is required (no systemctl found)."
[ "$#" -gt 0 ] || usage 1

systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1 ||
    warn "no ${SERVICE_NAME}.service installed yet — writing the drop-in anyway"

# ── Collect the requested paths ──────────────────────────────────────────────
declare -a REQUESTED=()
for raw in "$@"; do
    case "$raw" in
        /*) ;;
        *)  raw="$(cd "$(dirname "$raw")" 2>/dev/null && pwd)/$(basename "$raw")" ||
                die "cannot resolve relative path: $raw" ;;
    esac
    # Strip a trailing slash so the same directory named two ways is one entry.
    [ "$raw" != "/" ] && raw="${raw%/}"
    REQUESTED+=("$raw")
done

# ── Sanity-check each path before granting it ────────────────────────────────
if [ "$MODE" = "add" ]; then
    for p in "${REQUESTED[@]}"; do
        if [ ! -d "$p" ]; then
            warn "$p does not exist yet — creating it"
            mkdir -p "$p" || die "could not create $p (is the drive mounted, and read-write?)"
        fi
        # A drive mounted read-only stays read-only no matter what the unit
        # says, and a folder owned by a desktop user is unreadable to the
        # service. Both surface here rather than as a confusing UI error later.
        if ! id -u "$SVC_USER" >/dev/null 2>&1; then
            warn "no '${SVC_USER}' user on this system — skipping the permission check."
            warn "  (Set SAND_USER= if the service runs as somebody else.)"
        elif ! writable_by_service "$p"; then
            warn "$p is not writable by user '${SVC_USER}'."
            warn "  Give it ownership:  sudo chown -R ${SVC_USER}:${SVC_USER} $p"
            warn "  ...or, for a drive mounted by your desktop session, mount it with"
            warn "  options that user can write (uid=, gid=, umask= for NTFS/exFAT)."
        fi
        if findmnt -no OPTIONS --target "$p" 2>/dev/null | tr ',' '\n' | grep -qx 'ro'; then
            warn "$p sits on a read-only mount — remount it read-write first:"
            warn "  sudo mount -o remount,rw \"\$(findmnt -no TARGET --target $p)\""
        fi
    done
fi

# ── Drop what the unit already grants ────────────────────────────────────────
# The unit covers the mount roots an external drive normally appears under, so
# most paths people bring here need no drop-in entry at all. Say so and stop
# rather than writing one that grants nothing new.
if [ "$MODE" = "add" ]; then
    declare -a NEEDED=()
    for want in "${REQUESTED[@]}"; do
        if covered_by_unit "$want"; then
            ok "$want is already writable — the unit grants ${COVERING_ROOT}"
            continue
        fi
        NEEDED+=("$want")
    done
    if [ "${#NEEDED[@]}" -eq 0 ]; then
        echo ""
        echo "Nothing to add. If a Local folder account there still will not connect,"
        echo "the cause is above this line — ownership or the mount — not the sandbox."
        exit 0
    fi
    REQUESTED=("${NEEDED[@]}")
fi

# ── Merge with what is already granted ───────────────────────────────────────
declare -a KEEP=()
while IFS= read -r existing; do
    [ -n "$existing" ] || continue
    skip=0
    for want in "${REQUESTED[@]}"; do
        [ "$existing" = "$want" ] && skip=1
    done
    [ "$skip" -eq 1 ] && continue
    KEEP+=("$existing")
done < <(current_paths)

declare -a FINAL=()
if [ "$MODE" = "remove" ]; then
    FINAL=("${KEEP[@]:-}")
else
    FINAL=("${KEEP[@]:-}" "${REQUESTED[@]}")
fi
# The :- above can leave one empty element behind on an empty array.
declare -a CLEAN=()
for p in "${FINAL[@]:-}"; do
    [ -n "$p" ] && CLEAN+=("$p")
done

# ── Write the drop-in ────────────────────────────────────────────────────────
mkdir -p "$DROPIN_DIR"
{
    echo "# Written by scripts/allow-local-path.sh. Paths a Local folder account"
    echo "# may use, on top of the data directory and mount roots the main unit"
    echo "# already grants."
    echo "#"
    echo "# ProtectSystem=strict makes everything else read-only to the service."
    echo "# The leading '-' means a path that is missing at boot (an external"
    echo "# drive that is not plugged in) does not stop the service from starting."
    echo "[Service]"
    for p in "${CLEAN[@]:-}"; do
        printf 'ReadWritePaths=-"%s"\n' "$p"
    done
} > "$DROPIN"

if [ "${#CLEAN[@]}" -eq 0 ]; then
    rm -f "$DROPIN"
    rmdir "$DROPIN_DIR" 2>/dev/null || true
    ok "no extra paths remain — removed ${DROPIN}"
else
    ok "wrote ${DROPIN}"
    for p in "${CLEAN[@]}"; do
        printf '     %s%s%s\n' "$C_DIM" "$p" "$C_OFF"
    done
fi

systemctl daemon-reload
if systemctl is-active --quiet "${SERVICE_NAME}.service"; then
    systemctl restart "${SERVICE_NAME}.service"
    ok "restarted ${SERVICE_NAME}.service"
else
    warn "${SERVICE_NAME}.service is not running — start it with: systemctl start ${SERVICE_NAME}"
fi

[ "$MODE" = "add" ] || exit 0

echo ""
echo "Now reconnect the Local folder account in the web UI (+ Connect → Local folder)."
echo ""
echo "Note: a drive plugged in after the service starts is picked up automatically,"
echo "but if a Local folder account starts failing after a re-plug, restarting the"
echo "service re-establishes the grant:  sudo systemctl restart ${SERVICE_NAME}"
