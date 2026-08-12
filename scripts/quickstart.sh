#!/usr/bin/env bash
#
# SAND Vault — Linux quick-start installer (Ubuntu / Debian / Raspberry Pi OS).
#
# One command, run as root, installs SAND Vault as a hardened systemd service:
#
# The product is "SAND Vault"; the service, user, install prefix, data directory
# and SAND_* variables all stay on `sand` so an existing install keeps upgrading
# in place rather than needing a migration.
#
#   curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash
#
# Two ways to get the binary — SAND_INSTALL picks one:
#
#   source   (default) clone the repo and build it here. Needs Node and Go at
#            build time (installed automatically if missing); works on any
#            architecture and can track any branch/tag/commit.
#   release  download the prebuilt static binary from a GitHub release. No
#            toolchain, no source tree, no compile — seconds instead of minutes
#            on a Raspberry Pi.
#
#            curl -fsSL …/quickstart.sh | sudo SAND_INSTALL=release bash
#
# Both modes produce the same thing: one static binary with the web client
# embedded, run by the same systemd unit, against the same vault. You can
# switch between them by re-running with a different SAND_INSTALL.
#
# It is deliberately *non-disruptive* and *data-safe* — re-run it any time to
# upgrade in place:
#
#   * Idempotent. Re-running only swaps in newer code; it never re-initialises
#     a vault or touches stored files.
#   * The vault lives at a stable path OUTSIDE the source tree ($DATA_DIR), so
#     cloning, rebuilding, or pulling can never clobber it.
#   * Every upgrade STOPS the service and snapshots the vault file BEFORE
#     swapping code in, so the backup is always taken against a quiesced vault.
#   * The new build is compiled (or the new binary downloaded) while the old
#     version keeps serving. If that fails, the running service is untouched.
#   * After restart we poll /api/health; if the new version is unhealthy we ROLL
#     BACK to the previous commit (source mode) or the previous binary (release
#     mode), restore the pre-upgrade vault snapshot, and restart — so a bad
#     upgrade self-heals to the last good state with its data.
#
# WHY THE VAULT SNAPSHOT MATTERS MORE HERE THAN IT LOOKS.  The vault file is
# not a cache — it is the only record of which cloud account holds which part
# of which file, and the only copy of the credentials for those accounts. The
# encrypted parts scattered across your providers are meaningless without it.
# Losing it loses everything; hence a snapshot before every single upgrade, and
# hence $DATA_DIR living well away from anything this script rewrites.
#
# The deployed artifact is a single static Go binary that embeds the built web
# client. Node is only needed at BUILD time (to compile the client with Vite);
# the running service has no Node, npm, or JS runtime dependency.
#
# Configure via environment variables (all optional):
#
#   SAND_INSTALL     source | release        where the binary comes from (default: source)
#   SAND_REPO        git URL to clone        (default: https://github.com/chinmay28/sand-vault.git)
#   SAND_REF         branch/tag/commit       (default: main; source mode)
#   SAND_RELEASE     latest | <tag>          release to install (default: latest; release mode)
#   SAND_USER        service system user     (default: sand)
#   SAND_PREFIX      install prefix          (default: /opt/sand; source → $PREFIX/src)
#   SAND_DATA_DIR    vault + backups dir     (default: /var/lib/sand)
#   PORT             port to listen on       (default: 8080)
#   HOST             bind address            (default: 127.0.0.1 — see the warning below)
#   INSTALL_NODE     auto | never            install Node 22 if missing/old (default: auto; build-time only)
#   INSTALL_GO       auto | never            install Go if missing/old (default: auto; build-time only)
#   BACKUP_KEEP      pre-upgrade backups kept (default: 10)
#
# A NOTE ON HOST.  This defaults to 127.0.0.1, not 0.0.0.0, and that is not
# timidity. SAND Vault's server is the one component that ever holds plaintext: it
# rebuilds a decrypted file in memory to answer a download, and it takes your
# vault password over the wire. On a bare HTTP listener both cross the network
# in the clear. Expose it deliberately, behind TLS — a reverse proxy
# (scripts/nginx-sand.conf) or Tailscale Serve — rather than by default.

set -euo pipefail

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[1;32m'; C_YELLOW=$'\033[1;33m'
  C_RED=$'\033[1;31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_BLUE=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_DIM=''; C_OFF=''
fi
log()  { printf '%s==>%s %s\n' "$C_BLUE" "$C_OFF" "$*"; }
ok()   { printf '%s ok %s %s\n' "$C_GREEN" "$C_OFF" "$*"; }
warn() { printf '%swarn%s %s\n' "$C_YELLOW" "$C_OFF" "$*" >&2; }
die()  { printf '%serr %s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }
step() { printf '\n%s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }

# ---------------------------------------------------------------------------
# Must be root (system-wide service + dedicated user)
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  die "Run as root: curl -fsSL .../quickstart.sh | sudo bash   (or: sudo ./scripts/quickstart.sh)"
fi
command -v systemctl >/dev/null 2>&1 || die "systemd is required (no systemctl found)."

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
INSTALL_MODE="${SAND_INSTALL:-source}"
case "$INSTALL_MODE" in
  source | release) ;;
  *) die "SAND_INSTALL must be 'source' or 'release' (got '$INSTALL_MODE')." ;;
esac
SAND_REPO="${SAND_REPO:-https://github.com/chinmay28/sand-vault.git}"
SAND_REF="${SAND_REF:-main}"
RELEASE_TAG="${SAND_RELEASE:-latest}"
SVC_USER="${SAND_USER:-sand}"
PREFIX="${SAND_PREFIX:-/opt/sand}"
DATA_DIR="${SAND_DATA_DIR:-/var/lib/sand}"
PORT="${PORT:-8080}"
HOST="${HOST:-127.0.0.1}"
INSTALL_NODE="${INSTALL_NODE:-auto}"
INSTALL_GO="${INSTALL_GO:-auto}"
BACKUP_KEEP="${BACKUP_KEEP:-10}"

SRC_DIR="$PREFIX/src"
VAULT_PATH="$DATA_DIR/vault.sand"
BACKUP_DIR="$DATA_DIR/backups"
SERVICE_NAME="sand"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
# Minimum Go release that can bootstrap the build; the go directive in go.mod
# pins the real toolchain, which Go fetches automatically.
GO_MIN_MINOR=22
GO_INSTALL_VERSION="1.25.0"
NODE_MIN_MAJOR=18

# If this script is being run from inside an existing checkout (sudo ./scripts/
# quickstart.sh) rather than piped from curl, build that checkout in place.
# Release mode never builds, so it ignores the surrounding checkout entirely.
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" >/dev/null 2>&1 && pwd)"
LOCAL_CHECKOUT=""
if [ "$INSTALL_MODE" = source ] && git -C "$SELF_DIR" rev-parse --show-toplevel >/dev/null 2>&1; then
  top="$(git -C "$SELF_DIR" rev-parse --show-toplevel)"
  if [ -f "$top/go.mod" ] && grep -q 'module github.com/chinmay28/sand-vault' "$top/go.mod" 2>/dev/null; then
    LOCAL_CHECKOUT="$top"
    SRC_DIR="$top"   # build & serve from where the user already cloned
  fi
fi

if [ "$INSTALL_MODE" = release ]; then
  # No source tree at all: the binary is the whole install.
  SERVER_BIN="$PREFIX/bin/sand"
  WORK_DIR="$PREFIX"
else
  SERVER_BIN="$SRC_DIR/sand"
  WORK_DIR="$SRC_DIR"
fi
# Kept for rollback: the binary the service was running before this run.
PREV_BIN="${SERVER_BIN}.prev"
STAGED_BIN="${SERVER_BIN}.new"

log "SAND Vault quick start"
printf '  %-10s %s\n' "install"  "$INSTALL_MODE$( [ "$INSTALL_MODE" = release ] && echo " ($RELEASE_TAG)" )"
if [ "$INSTALL_MODE" = release ]; then
  printf '  %-10s %s\n' "binary"  "$SERVER_BIN"
else
  printf '  %-10s %s\n' "source"  "$SRC_DIR"
fi
printf '  %-10s %s\n' "data"     "$DATA_DIR"
printf '  %-10s %s\n' "vault"    "$VAULT_PATH"
printf '  %-10s %s\n' "service"  "${SERVICE_NAME}.service (user: $SVC_USER)"
printf '  %-10s %s\n' "listen"   "http://$HOST:$PORT"

# Run npm/git/go as the service user so the tree stays owned by them, and so the
# build matches the runtime account. Falls back to plain exec before the user exists.
as_svc() {
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    # Build needs devDependencies → make sure NODE_ENV isn't 'production'.
    sudo -u "$SVC_USER" --preserve-env=PATH env -u NODE_ENV "$@"
  else
    env -u NODE_ENV "$@"
  fi
}

# ---------------------------------------------------------------------------
# 1. Prerequisites
# ---------------------------------------------------------------------------
step "[1/7] Prerequisites"

APT=0; command -v apt-get >/dev/null 2>&1 && APT=1
ensure_pkg() {
  command -v "$1" >/dev/null 2>&1 && return 0
  [ "$APT" -eq 1 ] || die "'$1' missing and no apt-get to install it. Install it and re-run."
  log "installing $1…"
  apt-get update -y >/dev/null
  apt-get install -y "$1" >/dev/null
}

ensure_pkg curl
ensure_pkg ca-certificates
[ "$INSTALL_MODE" = source ] && ensure_pkg git
ok "curl, ca-certificates$( [ "$INSTALL_MODE" = source ] && echo ", git" ) present"

# Node and Go are build-time only — release mode needs neither.
if [ "$INSTALL_MODE" = source ]; then
  node_major() { node -v 2>/dev/null | sed 's/^v\([0-9]*\).*/\1/'; }
  if [ "$(node_major || echo 0)" -lt "$NODE_MIN_MAJOR" ] 2>/dev/null; then
    [ "$INSTALL_NODE" = auto ] || die "Node >= $NODE_MIN_MAJOR required and INSTALL_NODE=never."
    [ "$APT" -eq 1 ] || die "Node >= $NODE_MIN_MAJOR required; install it and re-run."
    log "installing Node 22 (build-time only)…"
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash - >/dev/null
    apt-get install -y nodejs >/dev/null
  fi
  ok "node $(node -v)"

  go_minor() { go version 2>/dev/null | sed -n 's/.*go1\.\([0-9]*\).*/\1/p'; }
  if [ "$(go_minor || echo 0)" -lt "$GO_MIN_MINOR" ] 2>/dev/null; then
    [ "$INSTALL_GO" = auto ] || die "Go >= 1.$GO_MIN_MINOR required and INSTALL_GO=never."
    log "installing Go $GO_INSTALL_VERSION (build-time only)…"
    go_arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
    case "$go_arch" in
      amd64 | x86_64) go_arch=amd64 ;;
      arm64 | aarch64) go_arch=arm64 ;;
      armhf | armv7l) go_arch=armv6l ;;
      *) die "unsupported architecture '$go_arch' for an automatic Go install." ;;
    esac
    tmp="$(mktemp -d)"
    curl -fsSL "https://go.dev/dl/go${GO_INSTALL_VERSION}.linux-${go_arch}.tar.gz" -o "$tmp/go.tgz"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
    export PATH="/usr/local/go/bin:$PATH"
    # Persist for the service user's later invocations and for interactive shells.
    printf 'export PATH=/usr/local/go/bin:$PATH\n' > /etc/profile.d/go.sh
  fi
  export PATH="/usr/local/go/bin:$PATH"
  ok "$(go version)"
fi

# ---------------------------------------------------------------------------
# 2. Service user
# ---------------------------------------------------------------------------
step "[2/7] Service user"
if id -u "$SVC_USER" >/dev/null 2>&1; then
  ok "user '$SVC_USER' exists"
else
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  ok "created system user '$SVC_USER'"
fi

# Is there already a service here? That makes this run an upgrade rather than
# a fresh install, which is what turns on snapshots and rollback.
UPGRADE=0
[ -f "$UNIT_PATH" ] && UPGRADE=1
PREV_SHA=""

# ---------------------------------------------------------------------------
# 3. Source or release
# ---------------------------------------------------------------------------
step "[3/7] Fetch"

release_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) die "no prebuilt binary for $(uname -m) — use the default source install." ;;
  esac
}

RELEASE_VERSION=""
if [ "$INSTALL_MODE" = release ]; then
  arch="$(release_arch)"
  api="https://api.github.com/repos/chinmay28/sand-vault/releases"
  if [ "$RELEASE_TAG" = latest ]; then
    api="$api/latest"
  else
    api="$api/tags/$RELEASE_TAG"
  fi

  meta="$(curl -fsSL "$api")" || die "could not reach the GitHub releases API."
  RELEASE_VERSION="$(printf '%s' "$meta" | sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$RELEASE_VERSION" ] || die "could not determine the release tag."

  asset="sand-${RELEASE_VERSION}-linux-${arch}"
  base="https://github.com/chinmay28/sand-vault/releases/download/${RELEASE_VERSION}"

  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  log "downloading $asset ($RELEASE_VERSION)…"
  curl -fsSL "$base/$asset" -o "$tmp/sand" \
    || die "no $asset in release $RELEASE_VERSION — that architecture may not be published; use the source install."

  # Verify before anything is swapped in. A corrupted or tampered download must
  # never reach the point where it can replace a working binary.
  if curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
    want="$(grep " $asset\$" "$tmp/SHA256SUMS" | awk '{print $1}' | head -1)"
    if [ -n "$want" ]; then
      got="$(sha256sum "$tmp/sand" | awk '{print $1}')"
      [ "$want" = "$got" ] || die "checksum mismatch for $asset (expected $want, got $got). Refusing to install."
      ok "checksum verified"
    else
      warn "SHA256SUMS has no entry for $asset — installing unverified."
    fi
  else
    warn "no SHA256SUMS published for $RELEASE_VERSION — installing unverified."
  fi

  install -d -m 755 "$(dirname "$SERVER_BIN")"
  install -m 755 "$tmp/sand" "$STAGED_BIN"
  ok "staged $RELEASE_VERSION → $STAGED_BIN"
else
  if [ -n "$LOCAL_CHECKOUT" ]; then
    ok "building the checkout at $SRC_DIR"
    [ -d "$SRC_DIR/.git" ] && PREV_SHA="$(git -C "$SRC_DIR" rev-parse HEAD 2>/dev/null || true)"
  elif [ -d "$SRC_DIR/.git" ]; then
    PREV_SHA="$(as_svc git -C "$SRC_DIR" rev-parse HEAD)"
    log "updating $SRC_DIR to $SAND_REF…"
    as_svc git -C "$SRC_DIR" fetch --prune origin
    as_svc git -C "$SRC_DIR" checkout -q -B deploy "origin/$SAND_REF" 2>/dev/null \
      || as_svc git -C "$SRC_DIR" checkout -q -B deploy "$SAND_REF"
    ok "at $(as_svc git -C "$SRC_DIR" rev-parse --short HEAD)"
  else
    install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$PREFIX"
    log "cloning $SAND_REPO…"
    # NOT --depth 1: the version's patch number is the commit count, so a
    # shallow clone would build something calling itself v2.0.1 forever.
    # blob:none keeps it cheap while still carrying the full commit graph.
    as_svc git clone --filter=blob:none --branch "$SAND_REF" "$SAND_REPO" "$SRC_DIR"
    ok "cloned to $SRC_DIR"
  fi
fi

# ---------------------------------------------------------------------------
# 4. Build (source mode only)
# ---------------------------------------------------------------------------
step "[4/7] Build"

build_src() {
  # Build the web client first — the Go binary embeds it, so the order matters.
  as_svc env PATH="$PATH" npm --prefix "$SRC_DIR/web" ci
  as_svc env PATH="$PATH" npm --prefix "$SRC_DIR/web" run build
  # Stamp the version: the patch number is the commit count, which only exists
  # here at build time. `make build-go` does the same thing.
  patch="$(as_svc node "$SRC_DIR/scripts/version.mjs" --patch 2>/dev/null || echo 0)"
  as_svc env PATH="$PATH" HOME="$SRC_DIR/.cache" \
    go -C "$SRC_DIR" build -trimpath \
      -ldflags "-s -w -X github.com/chinmay28/sand-vault/internal/version.Patch=${patch}" \
      -o "$STAGED_BIN" ./cmd/sand
}

if [ "$INSTALL_MODE" = source ]; then
  # Cache dirs the service user can actually write to (no home directory).
  install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$SRC_DIR/.cache"
  chown -R "$SVC_USER":"$SVC_USER" "$SRC_DIR" 2>/dev/null || true
  # The build runs while the OLD binary keeps serving. A failure here leaves
  # the running service completely untouched.
  build_src
  ok "built $("$STAGED_BIN" version 2>/dev/null || echo "sand")"
else
  ok "no build needed (prebuilt release)"
fi

# ---------------------------------------------------------------------------
# 5. Data dir + pre-upgrade vault snapshot
# ---------------------------------------------------------------------------
step "[5/7] Data directory + backup"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 750 "$DATA_DIR" "$BACKUP_DIR"
ok "data dir ready ($DATA_DIR, owned by $SVC_USER)"

stop_service()  { systemctl stop  "${SERVICE_NAME}.service" 2>/dev/null || true; }
start_service() { systemctl start "${SERVICE_NAME}.service"; }

SNAP=""
if [ "$UPGRADE" -eq 1 ] && [ -f "$VAULT_PATH" ]; then
  # Quiesce first so the snapshot can't catch a half-written vault. The server
  # writes it atomically, but stopping costs nothing and removes the question.
  stop_service
  ts="$(date +%Y%m%d-%H%M%S)"
  SNAP="$BACKUP_DIR/vault-$ts.sand"
  cp "$VAULT_PATH" "$SNAP"
  chown "$SVC_USER":"$SVC_USER" "$SNAP" 2>/dev/null || true
  chmod 600 "$SNAP"
  ok "vault backed up → $SNAP"
  # Prune, keeping the newest $BACKUP_KEEP.
  if [ "$BACKUP_KEEP" -gt 0 ]; then
    ls -1t "$BACKUP_DIR"/vault-*.sand 2>/dev/null | tail -n +"$((BACKUP_KEEP + 1))" | while read -r old; do
      rm -f "$old"
    done
  fi
fi

# ---------------------------------------------------------------------------
# 6. systemd unit + (re)start
# ---------------------------------------------------------------------------
step "[6/7] systemd service"

# The service is quiesced by now on an upgrade, so this is where the staged
# binary replaces the running one (keeping the old one for rollback).
install_staged() {
  [ -f "$STAGED_BIN" ] || return 0
  stop_service
  [ -f "$SERVER_BIN" ] && cp -f "$SERVER_BIN" "$PREV_BIN"
  mv -f "$STAGED_BIN" "$SERVER_BIN"
  chown "$SVC_USER":"$SVC_USER" "$SERVER_BIN" 2>/dev/null || true
  chmod 755 "$SERVER_BIN"
}
install_staged

write_unit() {
  cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=SAND Vault — split, encrypt and scatter files across cloud accounts
Documentation=https://github.com/chinmay28/sand
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
WorkingDirectory=$WORK_DIR
ExecStart=$SERVER_BIN serve --port $PORT --bind $HOST --vault $VAULT_PATH
Environment=SAND_VAULT=$VAULT_PATH
Restart=on-failure
RestartSec=3

# Hardening. The vault holds cloud credentials and the map of every stored
# file, so the service gets write access to exactly one directory and nothing
# else — note ProtectHome, which is also why the vault must live in
# $DATA_DIR rather than the service user's (non-existent) home.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
UNIT
}
write_unit
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
start_service
ok "service enabled and started"

# ---------------------------------------------------------------------------
# 7. Health check (with rollback on a failed upgrade)
# ---------------------------------------------------------------------------
step "[7/7] Health check"
health_url="http://127.0.0.1:$PORT/api/health"
check_health() {
  for _ in $(seq 1 30); do
    curl -fsS "$health_url" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# Restore the pre-upgrade vault snapshot, so the version we roll back to sees
# an index it understands.
restore_snapshot() {
  if [ -n "$SNAP" ] && [ -f "$SNAP" ]; then
    cp "$SNAP" "$VAULT_PATH"
    chown "$SVC_USER":"$SVC_USER" "$VAULT_PATH" 2>/dev/null || true
    chmod 600 "$VAULT_PATH"
  fi
}

if check_health; then
  ok "healthy ($health_url) — $(curl -fsS "$health_url" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p')"
elif [ "$INSTALL_MODE" = release ] && [ "$UPGRADE" -eq 1 ] && [ -f "$PREV_BIN" ]; then
  # Release-mode rollback: the previous binary is right there, so put it back
  # with the pre-upgrade vault and restart.
  warn "$RELEASE_VERSION failed its health check."
  warn "rolling back to the previous binary and restoring the pre-upgrade vault…"
  stop_service
  restore_snapshot
  mv -f "$PREV_BIN" "$SERVER_BIN"
  chown "$SVC_USER":"$SVC_USER" "$SERVER_BIN" 2>/dev/null || true
  start_service
  if check_health; then
    die "Upgrade to $RELEASE_VERSION failed its health check — rolled back to $("$SERVER_BIN" version 2>/dev/null || echo "the previous binary") with your vault intact. Check: journalctl -u ${SERVICE_NAME} -n 80"
  fi
  die "Upgrade AND rollback both failed health checks. Your vault snapshot is safe at $SNAP. Inspect: journalctl -u ${SERVICE_NAME} -n 80"
else
  warn "new version failed its health check."
  if [ "$UPGRADE" -eq 1 ] && [ -n "$PREV_SHA" ] && [ -z "$LOCAL_CHECKOUT" ]; then
    warn "rolling back to ${PREV_SHA:0:12} and restoring the pre-upgrade vault…"
    stop_service
    restore_snapshot
    as_svc git -C "$SRC_DIR" checkout -q -B deploy "$PREV_SHA"
    build_src
    install_staged
    start_service
    if check_health; then
      die "Upgrade failed health check — rolled back to ${PREV_SHA:0:12} with your vault intact. Check: journalctl -u ${SERVICE_NAME} -n 80"
    fi
    die "Upgrade AND rollback both failed health checks. Your vault snapshot is safe at $SNAP. Inspect: journalctl -u ${SERVICE_NAME} -n 80"
  fi
  die "Service is not healthy. Inspect logs: journalctl -u ${SERVICE_NAME} -n 80 --no-pager"
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
lan_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"; [ -n "$lan_ip" ] || lan_ip="<this-host>"
verb="installed"; [ "$UPGRADE" -eq 1 ] && verb="upgraded"

if [ "$INSTALL_MODE" = release ]; then
  origin_line="Installed:   $RELEASE_VERSION, prebuilt from the $RELEASE_TAG release (no toolchain needed)"
  upgrade_line="Upgrade:     re-run with SAND_INSTALL=release for the next release."
else
  origin_line="Source:      $SRC_DIR (built here)"
  upgrade_line="Upgrade:     re-run this script — it swaps code in, backs up your vault, self-heals."
fi

if [ "$HOST" = "127.0.0.1" ]; then
  reach_line="Open it:     http://localhost:$PORT   (loopback only — see below)"
else
  reach_line="Open it:     http://$lan_ip:$PORT"
fi

cat <<DONE

${C_GREEN}SAND Vault $verb and running.${C_OFF}

  $reach_line
  Vault:       $VAULT_PATH
  Backups:     $BACKUP_DIR
  Binary:      $SERVER_BIN (static; embeds the web client)
  $origin_line
  $upgrade_line

  First run? Create the vault and connect your accounts:
    $SERVER_BIN --vault $VAULT_PATH vault init
    $SERVER_BIN --vault $VAULT_PATH remote kinds

  Manage the service:
    systemctl status  ${SERVICE_NAME}
    systemctl restart ${SERVICE_NAME}
    journalctl -u ${SERVICE_NAME} -f
${C_DIM}
  BACK UP $VAULT_PATH. It is the only record of which account holds which part
  of which file, and the only copy of your cloud credentials. The encrypted
  parts sitting on your providers cannot be rebuilt without it.
DONE

if [ "$HOST" = "127.0.0.1" ]; then
  cat <<NOTE
  Bound to loopback. SAND Vault takes your vault password over this connection and
  sends rebuilt, decrypted files back over it, so put TLS in front before
  exposing it — Tailscale Serve, or a reverse proxy (scripts/nginx-sand.conf).
  Then re-run with HOST=0.0.0.0.${C_OFF}
NOTE
else
  cat <<NOTE
  Bound to $HOST — your vault password and decrypted files cross the network in
  the clear unless something in front of this is terminating TLS. See
  scripts/nginx-sand.conf.${C_OFF}
NOTE
fi
