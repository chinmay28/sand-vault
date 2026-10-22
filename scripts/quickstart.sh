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
# and the same command with a flag takes it off again, keeping the vault:
#
#   curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash -s -- --uninstall
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
#   * "Newer code" means origin/$SAND_REF — main unless you say otherwise. The
#     command at the top of this file deploys main from whatever directory you
#     happen to be standing in, including a clone of this repo.
#   * The one exception is EXECUTING this file from a checkout (sudo ./scripts/
#     quickstart.sh), which builds that checkout exactly as it stands and never
#     pulls — that is how you deploy work in progress. Every run prints which
#     of the two it is doing, and says so if the checkout is behind.
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
#   SAND_MOUNT_ROOTS mount roots a Local folder account may live under,
#                    colon-separated (default: /media:/run/media:/mnt:/srv).
#                    The unit is sandboxed (ProtectSystem=strict) and grants
#                    these, so an external disk mounted in the usual place is
#                    writable to the service; everything else stays read-only.
#                    Set empty to grant nothing but the data directory.
#   SAND_LOCAL_PATHS extra dirs a Local folder account may use, colon-separated
#                    — for a vault folder outside the mount roots above. Can
#                    also be granted later with scripts/allow-local-path.sh.
#   PORT             port to listen on       (default: 8123)
#   HOST             bind address            (default: 0.0.0.0 — see the warning below)
#   SAND_WEBDAV      1 | 0                   also serve the vault as a mountable
#                    WebDAV share at /dav (default: 0). Mount it with any
#                    username and the vault password. It sends that password on
#                    every request rather than once at sign-in, so put TLS in
#                    front before turning it on anywhere but loopback.
#
# PORT, HOST, SAND_WEBDAV, SAND_USER, SAND_PREFIX, SAND_DATA_DIR and
# SAND_MOUNT_ROOTS are remembered. On an upgrade, leaving one unset keeps
# whatever the service is already running with rather than resetting it to the
# default — so re-running this script to pick up a new version cannot quietly
# move a loopback-only install back onto every interface, or point the service
# at a vault that is not where the install put it. Setting one of the four
# paths to something OTHER than what the installed service uses is refused,
# with both values printed, unless the move is confirmed:
#
#   SAND_FORCE_PATHS 1                      point the service at the paths the
#                    environment names even though the installed unit differs.
#                    Nothing is moved: the vault, if any, stays at the old path.
#   INSTALL_NODE     auto | never            install Node 22 if missing/old (default: auto; build-time only)
#   INSTALL_GO       auto | never            install Go if missing/old (default: auto; build-time only)
#   BACKUP_KEEP      pre-upgrade backups kept (default: 10)
#
#   INSTALL_PROTON   auto | never            build Proton's Drive client so a Proton
#                    account can be connected without the desktop app (default: auto).
#                    Unlike Node and Go this is NOT build-time only — the client is
#                    what the service runs to reach Proton, so it stays installed.
#                    It needs bun, which has no build for 32-bit ARM; on such a host
#                    this step says so and skips rather than failing the install.
#   PROTON_CLI_URL   URL of a prebuilt proton-drive binary. Set this to skip the
#                    build entirely — on a Raspberry Pi it is the difference between
#                    seconds and twenty minutes. Get the URL for your platform from
#                    https://proton.me/download/drive/cli/index.html
#   PROTON_SDK_REF   branch/tag/commit of github.com/ProtonDriveApps/sdk to build
#                    (default: main)
#   PROTON_BUILD_TIMEOUT  minutes the compile may take before it is given up on
#                    (default: 45). The client is built LAST, after the service is
#                    up and healthy, so a slow or stuck build costs the Proton
#                    backend and nothing else; re-running picks it up again.
#
# Everything this prints is also appended to $SAND_PREFIX/quickstart.log, and a
# terminal that goes away mid-run (an ssh session dropped, a runner that timed
# out) does not stop the install: look there for how it ended.
#
# A NOTE ON HOST.  This defaults to 0.0.0.0, so the service is reachable from
# the rest of your network as soon as it is installed. Understand what you are
# exposing: this server is the one component that ever holds plaintext — it
# rebuilds a decrypted file in memory to answer a download, and it takes your
# vault password over the wire — and /api/vault/unlock answers anyone who can
# reach the port. On a bare HTTP listener the password and every file you open
# cross the network in the clear. Put TLS in front of it: a reverse proxy
# (scripts/nginx-sand.conf) or Tailscale Serve. Set HOST=127.0.0.1 to keep it
# on loopback until you have.

set -euo pipefail

# A dropped terminal must not kill a half-done install. This is usually run
# over ssh, to a Pi, and the session can go away for a dozen reasons that have
# nothing to do with the box — a laptop lid, a runner's timeout, wifi. The
# shell would then get a SIGHUP, or a SIGPIPE on its next line of output, and
# die wherever it happened to be: between stopping the service and starting the
# new one, if it was unlucky, leaving the box with no service at all. So both
# are ignored, every line printed here tolerates a dead stdout, and everything
# is also written to $QUICKSTART_LOG (set below, once the prefix is known),
# which is where to look when the session that started this is gone.
trap '' HUP PIPE

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[1;32m'; C_YELLOW=$'\033[1;33m'
  C_RED=$'\033[1;31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_BLUE=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_DIM=''; C_OFF=''
fi
# `|| :` on every print: with the terminal gone a write fails, and under set -e
# a failed printf would end the install on a progress line.
log()  { printf '%s==>%s %s\n' "$C_BLUE" "$C_OFF" "$*" 2>/dev/null || :; }
ok()   { printf '%s ok %s %s\n' "$C_GREEN" "$C_OFF" "$*" 2>/dev/null || :; }
warn() { printf '%swarn%s %s\n' "$C_YELLOW" "$C_OFF" "$*" >&2 2>/dev/null || :; }
die()  { printf '%serr %s %s\n' "$C_RED" "$C_OFF" "$*" >&2 2>/dev/null || :; exit 1; }
step() { printf '\n%s%s%s\n' "$C_DIM" "$*" "$C_OFF" 2>/dev/null || :; }

# ---------------------------------------------------------------------------
# Must be root (system-wide service + dedicated user)
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  die "Run as root: curl -fsSL .../quickstart.sh | sudo bash   (or: sudo ./scripts/quickstart.sh)"
fi

# Read before anything else happens, so that an uninstall never installs a
# toolchain, clones a repository or downloads a binary on its way out.
case "${1:-}" in
  --uninstall) UNINSTALL=1 ;;
  "")          UNINSTALL=0 ;;
  *)           die "Unknown option: $1 (only --uninstall is supported)" ;;
esac
# An uninstall on a box with no systemd has no service to stop, but still has
# files to remove, so only an install insists on it.
if [ "$UNINSTALL" = 0 ]; then
  command -v systemctl >/dev/null 2>&1 || die "systemd is required (no systemctl found)."
fi

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
SERVICE_NAME="sand"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
DEFAULT_MOUNT_ROOTS="/media:/run/media:/mnt:/srv"

# How the service is already running, if it is. The unit is rewritten from
# scratch on every run, so without reading it back an upgrade would silently
# reset anything the env did not name again — re-running this script to pick up
# a new version would move a loopback-only install back onto every interface,
# which is the last thing an upgrade should do quietly. An unset variable
# therefore means "keep what it runs with now", and only a fresh install falls
# through to the defaults below.
#
# That goes for the PATHS as much as for the port. The one-liner at the top of
# this file is how people upgrade, and it carries no environment: an install
# made with SAND_DATA_DIR=/srv/sand, upgraded that way, used to get a unit
# pointing at /var/lib/sand/vault.sand — a file that did not exist, so no
# snapshot was taken, the service came up offering to create a new vault, and
# the real one sat orphaned at the old path. Likewise a custom SAND_USER,
# SAND_PREFIX or SAND_MOUNT_ROOTS silently went back to the default. So the
# unit's User=, WorkingDirectory=, --vault and ReadWritePaths= are read back
# here, before anything else is decided, and an explicit variable that
# DISAGREES with the installed service is refused rather than acted on — see
# check_against_unit below.
PRIOR_EXEC=""
PRIOR_USER=""
PRIOR_WORKDIR=""
PRIOR_VAULT=""
PRIOR_DATA_DIR=""
PRIOR_ROOTS=""
PRIOR_ROOTS_KNOWN=0

# The value of a Key= line in the installed unit, or nothing.
unit_value() {
  [ -f "$UNIT_PATH" ] || return 0
  sed -n "s/^$1=//p" "$UNIT_PATH" | head -n 1
}
# The value of --flag in the running unit, or nothing.
prior_flag() {
  printf '%s' "$PRIOR_EXEC" | sed -n "s/.*--$1[= ]\([^ ]*\).*/\1/p" | head -n 1
}
# Whether a valueless --flag is in the running unit.
prior_switch() {
  case " $PRIOR_EXEC " in
    *" --$1 "* | *" --$1="*) return 0 ;;
    *) return 1 ;;
  esac
}
# The paths a unit or drop-in grants, one per line, stripped of the systemd
# syntax around them: the leading '-' that says a missing path must not fail
# the unit, and the quotes that keep a path with a space in it as one path.
# The same reading scripts/allow-local-path.sh does of the same files.
granted_paths() {
  [ -f "$1" ] || return 0
  grep -E '^ReadWritePaths=' "$1" 2>/dev/null |
    sed -e 's/^ReadWritePaths=//' -e 's/^-//' -e 's/^"//' -e 's/"$//' |
    grep -v '^$' || true
}
# The install prefix the unit was written for, or nothing. Read off the binary
# it runs, which this script only ever puts at $PREFIX/bin/sand (release mode)
# or $PREFIX/src/sand (source mode) — and only when WorkingDirectory= agrees,
# which rules out a unit something else wrote (scripts/deploy-linux.sh runs
# /usr/local/bin/sand, and /usr/local is nobody's prefix) and one an older
# version of this script wrote for a binary built inside somebody's checkout.
prior_prefix() {
  bin="${PRIOR_EXEC%% *}"
  case "$bin" in
    */bin/sand) p="${bin%/bin/sand}" ;;
    */src/sand) p="${bin%/src/sand}" ;;
    *) return 0 ;;
  esac
  case "$PRIOR_WORKDIR" in
    "$p" | "$p/src") printf '%s' "$p" ;;
  esac
}

if [ -f "$UNIT_PATH" ]; then
  PRIOR_EXEC="$(unit_value ExecStart)"
  PRIOR_USER="$(unit_value User)"
  PRIOR_WORKDIR="$(unit_value WorkingDirectory)"
  PRIOR_VAULT="$(prior_flag vault)"
  [ -n "$PRIOR_VAULT" ] || PRIOR_VAULT="$(unit_value Environment=SAND_VAULT)"
  [ -n "$PRIOR_VAULT" ] && PRIOR_DATA_DIR="$(dirname "$PRIOR_VAULT")"
  # The mount roots. A unit this script wrote names them on a comment line,
  # which is the only way "none" (SAND_MOUNT_ROOTS set empty) can be told from
  # a unit written before there were any. Failing that, they are whatever the
  # unit grants besides the data directory.
  if grep -q '^# SAND_MOUNT_ROOTS=' "$UNIT_PATH"; then
    PRIOR_ROOTS="$(sed -n 's/^# SAND_MOUNT_ROOTS=//p' "$UNIT_PATH" | head -n 1)"
    PRIOR_ROOTS_KNOWN=1
  else
    PRIOR_ROOTS="$(granted_paths "$UNIT_PATH" | grep -vx -- "$PRIOR_DATA_DIR" | paste -sd: - || true)"
    [ -n "$PRIOR_ROOTS" ] && PRIOR_ROOTS_KNOWN=1
  fi
fi

# An explicit variable that names a different path from the one the installed
# service uses is almost always a mistake — the usual one being an upgrade run
# with a stale shell history — and acting on it is how a vault gets orphaned.
# So it is refused, with both values printed, unless SAND_FORCE_PATHS=1 says
# the move is meant. Nothing is moved either way: with the override, the
# service is simply pointed at the new path, and whatever is at the old one
# stays there for the person who asked.
case "${SAND_FORCE_PATHS:-0}" in
  1 | true | yes | on) FORCE_PATHS=1 ;;
  *) FORCE_PATHS=0 ;;
esac
PATH_CONFLICTS=""
check_against_unit() {
  # check_against_unit VARIABLE value-this-run value-from-the-unit
  [ -n "$3" ] && [ "$2" != "$3" ] || return 0
  PATH_CONFLICTS="${PATH_CONFLICTS}  $1=$2   (the installed service uses $3)"$'\n'
}

SVC_USER="${SAND_USER:-${PRIOR_USER:-sand}}"
PREFIX="${SAND_PREFIX:-$(prior_prefix)}"
PREFIX="${PREFIX:-/opt/sand}"
DATA_DIR="${SAND_DATA_DIR:-${PRIOR_DATA_DIR:-/var/lib/sand}}"
[ -n "${SAND_USER:-}" ]     && check_against_unit SAND_USER     "$SVC_USER" "$PRIOR_USER"
[ -n "${SAND_PREFIX:-}" ]   && check_against_unit SAND_PREFIX   "$PREFIX"   "$(prior_prefix)"
[ -n "${SAND_DATA_DIR:-}" ] && check_against_unit SAND_DATA_DIR "$DATA_DIR" "$PRIOR_DATA_DIR"
if [ -n "$PATH_CONFLICTS" ]; then
  if [ "$FORCE_PATHS" = 1 ]; then
    warn "moving the install as asked (SAND_FORCE_PATHS=1):"
    printf '%s' "$PATH_CONFLICTS" >&2
    warn "nothing is moved: the service is pointed at the new paths, and the old ones are left as they are."
  else
    printf '%serr %s the environment names paths the installed service does not use:\n' "$C_RED" "$C_OFF" >&2
    printf '%s' "$PATH_CONFLICTS" >&2
    printf '     Re-run without those variables to keep the install as it is — an upgrade\n' >&2
    printf '     reads them back from %s — or with SAND_FORCE_PATHS=1 to\n' "$UNIT_PATH" >&2
    printf '     point the service at the new paths. The vault is not moved either way.\n' >&2
    exit 1
  fi
fi

# The mount roots a Local folder account may live under (see write_unit). The
# variable set EMPTY is a choice — a unit that grants nothing but the data
# directory — so it is tested for being set rather than for having a value.
if [ -n "${SAND_MOUNT_ROOTS+set}" ]; then
  MOUNT_ROOTS="$SAND_MOUNT_ROOTS"
elif [ "$PRIOR_ROOTS_KNOWN" = 1 ]; then
  MOUNT_ROOTS="$PRIOR_ROOTS"
else
  MOUNT_ROOTS="$DEFAULT_MOUNT_ROOTS"
fi

# Everything from here on is also written to this file, appended, so that an
# install whose terminal went away still has a record — and so "what did it
# say?" has an answer after the fact. tee is told to shrug off a closed pipe on
# the terminal side (--output-error=warn-nopipe) and carry on to the file; an
# older tee without that option is left out rather than risked, since a tee
# that dies would take the install's stdout with it.
QUICKSTART_LOG="$PREFIX/quickstart.log"
if [ "$UNINSTALL" = 0 ] && install -d -m 755 "$PREFIX" 2>/dev/null \
   && tee --output-error=warn-nopipe </dev/null >/dev/null 2>&1; then
  exec > >(tee -a --output-error=warn-nopipe "$QUICKSTART_LOG") 2>&1
  printf '\n--- quickstart %s ---\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$QUICKSTART_LOG" 2>/dev/null || :
  log "a copy of everything below is kept in $QUICKSTART_LOG"
fi

PORT="${PORT:-$(prior_flag port)}"
PORT="${PORT:-8123}"
HOST="${HOST:-$(prior_flag bind)}"
HOST="${HOST:-0.0.0.0}"

# The WebDAV share, off unless asked for. It is a second way in, authenticated
# by the vault password on every request rather than once at sign-in, so a bare
# HTTP listener exposes the password far more than the browser does.
if [ -n "${SAND_WEBDAV:-}" ]; then
  case "$SAND_WEBDAV" in
    1 | true | yes | on) WEBDAV=1 ;;
    0 | false | no | off) WEBDAV=0 ;;
    *) die "SAND_WEBDAV must be 1 or 0 (got '$SAND_WEBDAV')." ;;
  esac
elif prior_switch webdav; then
  WEBDAV=1
else
  WEBDAV=0
fi
WEBDAV_ARGS=""
[ "$WEBDAV" = 1 ] && WEBDAV_ARGS=" --webdav"

INSTALL_NODE="${INSTALL_NODE:-auto}"
INSTALL_GO="${INSTALL_GO:-auto}"
BACKUP_KEEP="${BACKUP_KEEP:-10}"

# Proton's own Drive client. SAND drives it to reach a Proton account without
# the desktop app, which is the only way a headless box can hold one — see
# internal/provider/protoncli.go. It is installed by default because the
# alternative is an account that connects, accepts parts and uploads none of
# them, which is the failure this backend exists to prevent.
INSTALL_PROTON="${INSTALL_PROTON:-auto}"
PROTON_SDK_REPO="${PROTON_SDK_REPO:-https://github.com/ProtonDriveApps/sdk.git}"
PROTON_SDK_REF="${PROTON_SDK_REF:-main}"
PROTON_CLI_URL="${PROTON_CLI_URL:-}"

SRC_DIR="$PREFIX/src"
# The service user is created with --no-create-home, so the home directory in
# its passwd entry does not exist and it has no way to create one. npm needs a
# writable HOME for its cache and logs — without it the install dies with
# `EACCES: permission denied, mkdir '/home/sand'` — and Go wants one for
# GOCACHE. Everything run as the service user therefore gets a HOME it owns.
BUILD_HOME="$PREFIX/.build-home"
# The vault the installed service already serves, when the data directory is
# the one it lives in; the conventional name under $DATA_DIR otherwise.
VAULT_PATH="$DATA_DIR/vault.sand"
[ "$PRIOR_DATA_DIR" = "$DATA_DIR" ] && [ -n "$PRIOR_VAULT" ] && VAULT_PATH="$PRIOR_VAULT"
BACKUP_DIR="$DATA_DIR/backups"

# Proton's client, the checkout it is built from, and the bun that builds it.
# The client goes on PATH via a symlink, so both the service and somebody
# running `sand remote proton login` by hand find the same binary.
PROTON_BIN="$PREFIX/bin/proton-drive"
PROTON_LINK="/usr/local/bin/proton-drive"
PROTON_SDK_DIR="$PREFIX/proton-sdk"
PROTON_STAMP="$PREFIX/.proton-cli-built"
BUN_DIR="$PREFIX/bun"
# Whichever route installs bun writes here, and its last lines are what a
# failure prints. Keeping it is the difference between "it did not work" and a
# report somebody can act on.
BUN_LOG="$PREFIX/bun-install.log"
PROTON_FETCH_LOG="$PREFIX/proton-fetch.log"
PROTON_BUILD_LOG="$PREFIX/proton-build.log"

# Where each Proton account keeps its client cache and, for the moment a
# command runs, its session. Under the data directory because that is the one
# path ProtectSystem=strict leaves writable — see write_unit.
PROTON_STATE_DIR="$DATA_DIR/proton"
# Minimum Go release that can bootstrap the build; the go directive in go.mod
# pins the real toolchain, which Go fetches automatically.
GO_MIN_MINOR=25
GO_INSTALL_VERSION="1.25.0"
NODE_MIN_MAJOR=18

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
#
# Undoes what the steps below put on the machine, from the same resolved paths:
# an install made with SAND_PREFIX, SAND_DATA_DIR or SAND_USER set is removed by
# an uninstall that reads them back from the unit, so the variables need naming
# again only when the unit is already gone. Every step tolerates its target
# being gone already, which is what lets it run twice, or on a box that never
# had SAND.
#
# What stays, deliberately:
#
#   * $DATA_DIR — the vault, its backups and Proton's client state. The vault
#     is the only map of which account holds which part of which file; deleting
#     it strands everything on the clouds. That is a decision for a person at a
#     prompt, not for an uninstaller, so the command is printed instead.
#   * The service user, which still owns $DATA_DIR.
#   * Any folder a Local folder account writes to. Those are the user's own
#     files; the grant that let the service write there goes with the drop-in.
#   * A checkout that `sudo ./scripts/quickstart.sh` built in place. It is the
#     user's working copy, not a tree this script cloned.
#   * Node, Go (/usr/local/go, /etc/profile.d/go.sh), git, curl, unzip. They
#     were installed only if missing, and something else may use them now.
if [ "$UNINSTALL" = 1 ]; then
  log "Uninstalling SAND Vault"
  # An empty or root prefix would turn the removals below into /bin and /src.
  case "$PREFIX" in
    "" | /) die "SAND_PREFIX='$PREFIX' is not a directory this script would have installed into." ;;
  esac

  # The binary the unit ran, read before the unit goes: when it is not under
  # $PREFIX it was built inside somebody's checkout, which is theirs to keep.
  prior_bin="${PRIOR_EXEC%% *}"

  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
  fi
  rm -f "$UNIT_PATH"
  # The drop-in directory holds the Local folder grants (SAND_LOCAL_PATHS,
  # scripts/allow-local-path.sh) and anything `systemctl edit sand` wrote. All
  # of it configures a unit that no longer exists.
  rm -rf "${UNIT_PATH}.d"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl reset-failed "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
  fi
  ok "service stopped and removed"

  # Only while it is still our link. A proton-drive somebody installed there
  # themselves is not this script's to take away.
  if [ -L "$PROTON_LINK" ] && [ "$(readlink "$PROTON_LINK")" = "$PROTON_BIN" ]; then
    rm -f "$PROTON_LINK"
    ok "removed $PROTON_LINK"
  fi

  # Named piece by piece rather than `rm -rf $PREFIX`: a data directory placed
  # under the prefix by SAND_DATA_DIR would otherwise go with it, and that is
  # the one thing this must never take.
  if [ -d "$PREFIX" ]; then
    rm -rf "${PREFIX:?}/src" "${PREFIX:?}/bin" "${PREFIX:?}/bun" "${PREFIX:?}/proton-sdk" \
           "$BUILD_HOME" "$BUN_LOG" "$PROTON_FETCH_LOG" "$PROTON_BUILD_LOG" \
           "$PROTON_STAMP"
    if rmdir "$PREFIX" 2>/dev/null; then
      ok "removed $PREFIX"
    else
      warn "kept $PREFIX — it still holds files this installer did not put there."
    fi
  fi

  if [ -n "$prior_bin" ] && [ "${prior_bin#"$PREFIX"/}" = "$prior_bin" ]; then
    warn "the service ran $prior_bin, built in a checkout — left as it is."
  fi

  echo
  if [ -d "$DATA_DIR" ]; then
    log "Removed. Your vault is still at $VAULT_PATH, with backups in $BACKUP_DIR."
    log "It is the only record of where your files' parts are. To delete it and the service user:"
    log "  sudo rm -rf $DATA_DIR && sudo userdel $SVC_USER"
  else
    log "Removed. There was no data directory at $DATA_DIR."
    if id -u "$SVC_USER" >/dev/null 2>&1; then
      log "To delete the service user as well: sudo userdel $SVC_USER"
    fi
  fi
  exit 0
fi

# Executed from inside a checkout (sudo ./scripts/quickstart.sh), build that
# checkout in place; piped from curl, deploy $SAND_REF like any other install.
# Release mode never builds, so it ignores the surrounding checkout entirely.
#
# Which of the two is happening has to be read off BASH_SOURCE, not $0. Piped,
# bash sets $0 to "bash" and leaves BASH_SOURCE unset, so the old
# `dirname ${BASH_SOURCE[0]:-$0}` collapsed to "." — the CURRENT DIRECTORY. It
# was not detecting "ran from a checkout" at all, only "stood in one", so
# curl … | sudo bash issued from a clone quietly built that clone instead of
# main, and kept reinstalling it long after the clone went stale. BASH_SOURCE
# is only a readable file when bash was given a script to run, which is exactly
# the distinction being drawn.
SELF_FILE="${BASH_SOURCE[0]:-}"
LOCAL_CHECKOUT=""
if [ "$INSTALL_MODE" = source ] && [ -f "$SELF_FILE" ]; then
  SELF_DIR="$(cd "$(dirname "$SELF_FILE")" >/dev/null 2>&1 && pwd)"
  if top="$(git -C "$SELF_DIR" rev-parse --show-toplevel 2>/dev/null)" \
     && [ -f "$top/go.mod" ] \
     && grep -q 'module github.com/chinmay28/sand-vault' "$top/go.mod" 2>/dev/null; then
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
  # What is about to be built, stated before anything is built. "Nothing
  # changed after a deploy" is a much harder thing to sit and wonder about
  # than a line saying which commit the deploy was made from.
  if [ -n "$LOCAL_CHECKOUT" ]; then
    printf '  %-10s %s\n' "ref"    "this checkout, built as it stands (not updated)"
  else
    printf '  %-10s %s\n' "ref"    "origin/$SAND_REF"
  fi
fi
printf '  %-10s %s\n' "data"     "$DATA_DIR"
printf '  %-10s %s\n' "vault"    "$VAULT_PATH"
printf '  %-10s %s\n' "service"  "${SERVICE_NAME}.service (user: $SVC_USER)"
printf '  %-10s %s\n' "listen"   "http://$HOST:$PORT"
[ "$WEBDAV" = 1 ] && printf '  %-10s %s\n' "webdav"   "http://$HOST:$PORT/dav/"

# Run npm/git/go as the service user so the tree stays owned by them, and so the
# build matches the runtime account. Falls back to plain exec before the user exists.
as_svc() {
  # sudo scrubs the environment, so everything the build genuinely needs has to
  # be handed over explicitly. PATH matters because a `Defaults secure_path` in
  # sudoers overrides --preserve-env=PATH and would hide a freshly installed Go;
  # the proxy and CA variables matter because without them npm and go cannot
  # reach the network at all on a host behind a corporate proxy.
  # GIT_TERMINAL_PROMPT=0 because nothing here can answer a prompt: a repo that
  # has moved or gone private makes git ask for a username, and an installer
  # fed to bash through a pipe would sit on that question forever.
  local -a passthru=("HOME=$BUILD_HOME" "PATH=$PATH" "GIT_TERMINAL_PROMPT=0")
  local v val
  for v in HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy \
           GOPROXY GOPRIVATE; do
    val="${!v-}"
    [ -n "$val" ] && passthru+=("$v=$val")
  done

  # A custom CA bundle is only useful if the service user can read it. Handing
  # over a path it cannot open makes things worse than saying nothing: node
  # warns, then fails TLS anyway, and the reason is buried.
  if [ -n "${NODE_EXTRA_CA_CERTS-}" ]; then
    if sudo -u "$SVC_USER" test -r "$NODE_EXTRA_CA_CERTS" 2>/dev/null; then
      passthru+=("NODE_EXTRA_CA_CERTS=$NODE_EXTRA_CA_CERTS")
    else
      warn "NODE_EXTRA_CA_CERTS ($NODE_EXTRA_CA_CERTS) is not readable by $SVC_USER — ignoring it."
      warn "If the build cannot reach the registry, make that file world-readable and re-run."
    fi
  fi

  # AS_SVC_TIMEOUT, when set, bounds the command in seconds: timeout(1) runs
  # inside the sudo so the whole process group — bun and everything it spawns —
  # is stopped together, and the caller sees 124. See run_watched.
  local -a limit=()
  if [ -n "${AS_SVC_TIMEOUT-}" ] && command -v timeout >/dev/null 2>&1; then
    limit=(timeout -k 15 "$AS_SVC_TIMEOUT")
  fi
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    # Build needs devDependencies → make sure NODE_ENV isn't 'production'.
    sudo -u "$SVC_USER" --preserve-env=PATH env -u NODE_ENV "${passthru[@]}" "${limit[@]}" "$@"
  else
    env -u NODE_ENV "${passthru[@]}" "${limit[@]}" "$@"
  fi
}

# ---------------------------------------------------------------------------
# 1. Prerequisites
# ---------------------------------------------------------------------------
step "[1/8] Prerequisites"

APT=0; command -v apt-get >/dev/null 2>&1 && APT=1
ensure_pkg() {
  command -v "$1" >/dev/null 2>&1 && return 0
  [ "$APT" -eq 1 ] || die "'$1' missing and no apt-get to install it. Install it and re-run."
  log "installing $1…"
  DEBIAN_FRONTEND=noninteractive apt-get update -y >/dev/null
  DEBIAN_FRONTEND=noninteractive apt-get install -y "$1" >/dev/null
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
step "[2/8] Service user"
if id -u "$SVC_USER" >/dev/null 2>&1; then
  ok "user '$SVC_USER' exists"
else
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  ok "created system user '$SVC_USER'"
fi

# Must exist before the first as_svc call (the clone in step 3 is one).
install -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$BUILD_HOME"

# Is there already a service here? That makes this run an upgrade rather than
# a fresh install, which is what turns on snapshots and rollback.
UPGRADE=0
[ -f "$UNIT_PATH" ] && UPGRADE=1
PREV_SHA=""

# ---------------------------------------------------------------------------
# 3. Source or release
# ---------------------------------------------------------------------------
step "[3/8] Fetch"

release_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) die "no prebuilt binary for $(uname -m) — use the default source install." ;;
  esac
}

# git in someone else's checkout, run as root. Without this, git refuses to
# touch a tree it does not own and every read below fails identically to "no
# repository here", which is the one answer that must not be guessed at.
src_git() { git -C "$SRC_DIR" -c safe.directory="$SRC_DIR" "$@"; }

# What the build is about to be made from, appended to the line announcing it.
describe_head() {
  [ -d "$SRC_DIR/.git" ] || return 0
  sha="$(src_git rev-parse --short HEAD 2>/dev/null)" || return 0
  printf ' (%s)' "$sha"
}

# Building a checkout in place is the whole point of this mode — it is how you
# deploy something you are still working on. But it also means a tree that was
# cloned once and never pulled rebuilds the same commit forever, and the only
# outward sign is a version number that will not move: the deploy succeeds, the
# service restarts, and nothing whatsoever changes. Say it out loud instead.
warn_if_behind() {
  [ -d "$SRC_DIR/.git" ] || return 0
  # Offline, or a repo with no origin — nothing to compare against, so this
  # tells us nothing either way and stays quiet.
  src_git fetch --quiet --prune origin 2>/dev/null || return 0
  src_git rev-parse --verify --quiet "origin/$SAND_REF" >/dev/null 2>&1 || return 0

  behind="$(src_git rev-list --count "HEAD..origin/$SAND_REF" 2>/dev/null || echo 0)"
  [ "${behind:-0}" -gt 0 ] || return 0

  warn "this checkout is $behind commit(s) behind origin/$SAND_REF."
  warn "quickstart builds what is here, so none of them will be installed."
  warn "to deploy them:  git -C '$SRC_DIR' pull --ff-only   (then re-run this)"
}

# Move the managed clone onto a commit, whatever the last build left lying in
# it.
#
# `git checkout` is the wrong verb here and would fail outright: the web build
# writes into internal/server/dist, which is TRACKED, so the second and every
# later deploy meets
#
#   error: Your local changes to the following files would be overwritten by
#   checkout: internal/server/dist/index.html
#
# and set -e ends the run — including the rollback path, where the whole point
# is to get back to something that works. This tree is a build artifact the
# script owns, not a working copy anyone edits, so a reset is honest about it.
#
# The clean is scoped to the build's output directory rather than the tree,
# because the running binary, the staged one and the rollback copy all sit in
# $SRC_DIR too, and a blanket `git clean -fd` would take the rollback copy with
# it. Stale hashed assets are worth removing: nothing references them, but
# go:embed puts every one of them in the binary.
deploy_to() {
  target="$1"
  as_svc git -C "$SRC_DIR" rev-parse --verify --quiet "${target}^{commit}" >/dev/null 2>&1 || return 1

  # Get onto the deploy branch without moving the tree — that always succeeds —
  # then move branch and tree together. This is the step `checkout -B deploy
  # <target>` was doing, minus its refusal to tread on the last build.
  as_svc git -C "$SRC_DIR" checkout -q -B deploy
  as_svc git -C "$SRC_DIR" reset -q --hard "$target"
  as_svc git -C "$SRC_DIR" clean -qfd -- internal/server/dist
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
    ok "building the checkout at $SRC_DIR$(describe_head)"
    [ -d "$SRC_DIR/.git" ] && PREV_SHA="$(src_git rev-parse HEAD 2>/dev/null || true)"
    warn_if_behind
  elif [ -d "$SRC_DIR/.git" ]; then
    PREV_SHA="$(as_svc git -C "$SRC_DIR" rev-parse HEAD)"
    log "updating $SRC_DIR to $SAND_REF…"
    as_svc git -C "$SRC_DIR" fetch --prune origin
    deploy_to "origin/$SAND_REF" || deploy_to "$SAND_REF"
    ok "at $(as_svc git -C "$SRC_DIR" rev-parse --short HEAD)"
  else
    install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$PREFIX"
    log "cloning $SAND_REPO…"
    # NOT --depth 1: the version's patch number is the commit count, so a
    # shallow clone would build something calling itself v2026.8.1 forever.
    # blob:none keeps it cheap while still carrying the full commit graph.
    as_svc git clone --filter=blob:none --branch "$SAND_REF" "$SAND_REPO" "$SRC_DIR"
    ok "cloned to $SRC_DIR"
  fi
fi

# ---------------------------------------------------------------------------
# 4. Build (source mode only)
# ---------------------------------------------------------------------------
step "[4/8] Build"

build_src() {
  # Build the web client first — the Go binary embeds it, so the order matters.
  #
  # `cd` rather than `npm --prefix`: --prefix is not honoured consistently for
  # `npm ci` across npm versions — some read package-lock.json from the working
  # directory regardless — and the resulting EUSAGE is a baffling way to fail.
  #
  # node_modules is cleared first so a re-run after a failed install starts from
  # a known state instead of a half-extracted tree.
  #
  # --no-audit --no-fund: an unattended installer must not fail because a
  # non-essential advisory lookup could not reach the registry.
  as_svc sh -c "cd '$SRC_DIR/web' && rm -rf node_modules && npm ci --no-audit --no-fund"
  as_svc sh -c "cd '$SRC_DIR/web' && npm run build"

  # Stamp the version: the patch number is the commit count and the line is the
  # commit's month, which only exist here at build time. `make build-go` does
  # the same thing.
  patch="$(as_svc node "$SRC_DIR/scripts/version.mjs" --patch 2>/dev/null || echo 0)"
  date="$(as_svc node "$SRC_DIR/scripts/version.mjs" --date 2>/dev/null || echo "")"
  as_svc go -C "$SRC_DIR" build -trimpath \
      -ldflags "-s -w -X github.com/chinmay28/sand-vault/internal/version.Patch=${patch} -X github.com/chinmay28/sand-vault/internal/version.Date=${date}" \
      -o "$STAGED_BIN" ./cmd/sand
}

if [ "$INSTALL_MODE" = source ]; then
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
step "[5/8] Data directory + backup"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 750 "$DATA_DIR" "$BACKUP_DIR"
# 700 rather than 750: a Proton account's session passes through here on its
# way to the client, and for those few moments it is the key material that
# unlocks the account rather than a token that expires. Nothing but the service
# user has any business in this directory.
install -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$PROTON_STATE_DIR"
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
step "[6/8] systemd service"

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

# ProtectSystem=strict in the unit makes the whole filesystem read-only to the
# service. Removable disks and network shares are mounted under a handful of
# well-known roots, so the unit grants those outright: a "Local folder" account
# on an external drive connects with no extra step, while /etc, /usr, /home and
# everything else stay read-only. SAND_MOUNT_ROOTS overrides the list; set it
# empty for a unit that grants nothing but $DATA_DIR. $MOUNT_ROOTS was settled
# in the configuration section, from the variable, the installed unit, or the
# default, in that order.
mount_root_lines() {
  # The trailing ':' gives the last root a newline of its own, so `read` sees
  # it. The leading '-' lets the service start on a host where a root does not
  # exist; the quotes keep a path with a space in it as one path.
  printf '%s:' "$MOUNT_ROOTS" | tr ':' '\n' | while IFS= read -r root; do
    [ -n "$root" ] || continue
    printf 'ReadWritePaths=-"%s"\n' "${root%/}"
  done
}
MOUNT_ROOT_LINES="$(mount_root_lines)"

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
ExecStart=$SERVER_BIN serve --port $PORT --bind $HOST --vault $VAULT_PATH$WEBDAV_ARGS
Environment=SAND_VAULT=$VAULT_PATH
# Where a Proton account's client keeps its cache, and its session for the
# moment a command runs. Without this the client falls back to a cache
# directory under a home the service user does not have, which
# ProtectSystem=strict leaves read-only — and a client that cannot write its
# cache cannot run at all.
Environment=SAND_PROTON_STATE_DIR=$PROTON_STATE_DIR
Restart=on-failure
RestartSec=3

# A ceiling, so that whatever SAND does it does to itself rather than to the
# machine. A percentage rather than a number, so it tracks whatever it lands on:
# 800 MB on a 1 GB Pi, 12.8 GB on a 16 GB one, without editing anything here.
#
# MemorySwapMax=0 is the half that keeps the box responsive. A limit met by
# swapping to an SD card is precisely the unresponsiveness the limit exists to
# prevent — better to be killed and restarted than to take ssh down with you.
#
# SAND reads this limit back at startup and sets GOMEMLIMIT under it, so the
# collector works against the ceiling rather than growing towards it. Raising
# this with 'systemctl edit sand' is enough; nothing else needs changing.
MemoryMax=80%
MemorySwapMax=0

# Hardening. The vault holds cloud credentials and the map of every stored
# file, so the service gets write access to its data directory and the mount
# roots a Local folder account lives under, and nothing else — note
# ProtectHome, which is also why the vault must live in $DATA_DIR rather than
# the service user's (non-existent) home.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
ReadWritePaths=$DATA_DIR
# The mount roots this unit was written with, as SAND_MOUNT_ROOTS had them.
# The next run of quickstart.sh reads this line back, so an upgrade keeps the
# list — including an empty one, which the ReadWritePaths= lines alone could
# not say. Edit the lines below rather than this one; they are what systemd
# reads.
# SAND_MOUNT_ROOTS=$MOUNT_ROOTS
$MOUNT_ROOT_LINES

[Install]
WantedBy=multi-user.target
UNIT
}
write_unit

# The unit grants $DATA_DIR and the mount roots above; ProtectSystem=strict
# keeps every other path read-only, so a "Local folder" account outside those
# roots cannot connect ("read-only file system") until its directory is granted
# too. SAND_LOCAL_PATHS grants such paths here, colon-separated;
# scripts/allow-local-path.sh does the same later. Both write this one drop-in.
#
# A re-run without SAND_LOCAL_PATHS leaves the drop-in alone, so grants survive
# upgrades. A re-run WITH it adds to what is there rather than replacing it:
# the drop-in is the one record of every path allow-local-path.sh has granted
# since the install, and an installer that rewrote it from the variable alone
# took those grants away — every Local folder account outside the mount roots
# went read-only on the next upgrade that happened to name a path.
DROPIN_DIR="${UNIT_PATH}.d"
LOCAL_PATHS_DROPIN="$DROPIN_DIR/10-local-paths.conf"

# What the drop-in should grant after this run: what it grants now, then the
# paths SAND_LOCAL_PATHS names, each once, in that order.
merged_local_paths() {
  {
    granted_paths "$LOCAL_PATHS_DROPIN"
    printf '%s:' "$SAND_LOCAL_PATHS" | tr ':' '\n' | while IFS= read -r entry; do
      [ -n "$entry" ] || continue
      [ "$entry" != / ] && entry="${entry%/}"
      printf '%s\n' "$entry"
    done
  } | awk '!seen[$0]++'
}

if [ -n "${SAND_LOCAL_PATHS:-}" ]; then
  # Read before the redirect below opens the file: `> file` truncates it
  # before the block runs, and what is being merged in is that file.
  LOCAL_PATHS_FINAL="$(merged_local_paths)"
  mkdir -p "$DROPIN_DIR"
  {
    echo "# Paths a Local folder account may use. Also managed by"
    echo "# scripts/allow-local-path.sh. A leading '-' lets the service start"
    echo "# when the drive is not plugged in."
    echo "[Service]"
    printf '%s\n' "$LOCAL_PATHS_FINAL" | while IFS= read -r entry; do
      [ -n "$entry" ] || continue
      printf 'ReadWritePaths=-"%s"\n' "$entry"
    done
  } > "$LOCAL_PATHS_DROPIN"
  ok "local paths granted → $LOCAL_PATHS_DROPIN"
  granted_paths "$LOCAL_PATHS_DROPIN" | sed 's/^/       /'
fi

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
start_service
ok "service enabled and started"

# ---------------------------------------------------------------------------
# 7. Health check (with rollback on a failed upgrade)
# ---------------------------------------------------------------------------
step "[7/8] Health check"

# The address to probe is the one the unit binds, not loopback. The probe used
# to go to 127.0.0.1 regardless of HOST, which was right for the default
# (0.0.0.0 answers on loopback too) and wrong for every other address: an
# install bound to a LAN or Tailscale address — the one thing the warning in
# the header asks for — failed this check on every run, a fresh install died
# "unhealthy", and an upgrade rolled a working service back to the previous
# commit and the pre-upgrade vault. A wildcard still maps to loopback, since a
# wildcard is not an address curl can connect to, and an IPv6 address goes in
# brackets, which is how a URL spells one.
probe_host() {
  case "$1" in
    "" | 0.0.0.0 | :: | "[::]") printf '127.0.0.1' ;;
    "["*"]") printf '%s' "$1" ;;
    *:*) printf '[%s]' "$1" ;;
    *) printf '%s' "$1" ;;
  esac
}
health_url="http://$(probe_host "$HOST"):$PORT/api/health"
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
    deploy_to "$PREV_SHA"
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
# 8. Proton Drive client
# ---------------------------------------------------------------------------
#
# Proton publishes no API a third party may use, and no Go SDK. What it does
# publish is the client its own apps are built on, so SAND drives that instead
# of reimplementing Proton's cryptography — which is just as well, since that
# cryptography changes at the end of 2026 and every client implementing only
# the old model stops working. Building Proton's binary puts that migration on
# Proton.
#
# This is not a build-time dependency like Node and Go. The client is what the
# running service executes to reach Proton, so it stays.
#
# Everything here is skippable and nothing here is fatal: a host that cannot
# build it gets a warning and an install that works in every other respect. A
# Proton account can still be connected as a synced folder.
#
# It comes LAST, after the service is installed, started and found healthy,
# because it is the slowest and least certain step and the service does not
# depend on it: the client is looked up on PATH each time it is run, so one
# that lands after the service is up is found the moment it is wanted. The
# other order held the whole install — and the service it was upgrading — behind
# a compile that, on a small board, could take an hour or not finish at all.
step "[8/8] Proton Drive client"

# bun_supported reports whether this machine has a bun to build with. bun ships
# 64-bit builds only, so a 32-bit Raspberry Pi cannot build the client — which
# is a thing to say plainly rather than a build to watch fail.
#
# dpkg is asked first because `uname -m` answers for the KERNEL, and a Pi
# running a 64-bit kernel over a 32-bit userland — the stock Raspberry Pi OS
# armhf image does exactly this — reports aarch64 while every binary on it is
# 32-bit. Trusting that downloads a bun that cannot run.
bun_arch() { dpkg --print-architecture 2>/dev/null || uname -m; }
bun_supported() {
  case "$(bun_arch)" in
    x86_64 | amd64 | aarch64 | arm64) return 0 ;;
    *) return 1 ;;
  esac
}

# install_bun fetches the runtime Proton's client is built with. It goes under
# $PREFIX rather than into the service user's home so that an upgrade can find
# it again, and so nothing ends up in /root when this is run under sudo.
# install_bun fetches bun — the version asked for, when one is, because Proton's
# build script refuses any bun but the one its package.json names (cli/
# package.json, "engines"), and bun moves faster than the SDK pins it: the day
# the registry's latest went past the pin, every fresh install started failing
# with "Bun 1.4.2 is required, but 1.4.3 is running". So the SDK is fetched
# first and asked, and a bun already here is replaced if it is the wrong one.
install_bun() {
  want="${1-}"
  # Present is not the same as working: a bun left behind by an install that
  # fetched the wrong architecture is a file that exists and cannot run, and
  # the whole point of checking here is to not discover that twenty minutes
  # into a build.
  if [ -x "$BUN_DIR/bin/bun" ] && version="$("$BUN_DIR/bin/bun" --version 2>/dev/null)"; then
    if [ -z "$want" ] || [ "$version" = "$want" ]; then
      ok "bun $version"
      return 0
    fi
    log "bun $version is here but Proton's client builds with $want; replacing it."
  fi
  log "installing bun${want:+ $want} (builds Proton's client)…"
  install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$BUN_DIR"

  # npm first, where there is one. It is faster than fetching and unpacking a
  # zip, it reports an unsupported platform as an error instead of installing
  # something that cannot run, and — the reason it is first rather than the
  # fallback — a source-mode install has already used the npm registry to build
  # the web client, so it is a route this machine is known to reach. bun.sh is
  # one more host that may not be.
  #
  # Release mode installs no Node, so npm may not be here at all; that is what
  # the second route is for.
  if command -v npm >/dev/null 2>&1 && install_bun_via_npm "$want"; then
    :
  else
    command -v npm >/dev/null 2>&1 && warn "npm could not install bun; trying bun.sh."
    install_bun_via_bunsh "$want" || return 1
  fi

  if [ ! -x "$BUN_DIR/bin/bun" ]; then
    warn "the bun install finished but left no binary at $BUN_DIR/bin/bun."
    warn "Last lines of $BUN_LOG:"
    tail -n 6 "$BUN_LOG" 2>/dev/null | sed 's/^/       /' >&2
    return 1
  fi
  # Captured with `|| status=$?` rather than tested with `if !`, because inside
  # the branch of a negated test $? is the negation's own status — 0 — and the
  # message would report every failure as a success. It also keeps set -e off
  # the assignment.
  status=0
  version="$("$BUN_DIR/bin/bun" --version 2>&1)" || status=$?
  if [ "$status" -ne 0 ]; then
    # A binary of the wrong architecture is killed by the kernel and may say
    # nothing at all, so there has to be something to print when it does not.
    [ -n "$version" ] || version="it exited $status without saying anything"
    warn "bun installed but will not run on this machine:"
    warn "  $version"
    warn "That usually means a 64-bit kernel over a 32-bit userland, which bun"
    warn "has no build for. Set PROTON_CLI_URL to a prebuilt proton-drive"
    warn "binary, or connect Proton as a synced folder."
    return 1
  fi
  ok "bun $version"
}

# install_bun_via_npm takes bun from the registry, which publishes it as a
# per-platform binary package. Everything downstream looks for $BUN_DIR/bin/bun,
# so the two routes are made to agree with a symlink rather than by teaching the
# rest of the script about npm's layout.
install_bun_via_npm() {
  want="${1-}"
  # npm refuses a --prefix with no package.json in it (ENOENT, and it names the
  # missing file rather than the problem). A private stub makes the directory a
  # place npm will install into, and keeps the install out of any package.json
  # further up the tree.
  [ -f "$BUN_DIR/package.json" ] || printf '{"name":"sand-bun","private":true}\n' > "$BUN_DIR/package.json"
  chown "$SVC_USER":"$SVC_USER" "$BUN_DIR/package.json" 2>/dev/null || true

  # cd into the directory rather than pointing --prefix at it. This script runs
  # from wherever it was invoked, and npm resolves a project from the working
  # directory: started inside somebody else's Node project, --prefix is not
  # enough to stop npm deciding that project is "up to date" and installing
  # nothing here — exiting 0 while leaving no bun behind.
  #
  # Bounded: a registry that does not answer makes npm retry for a long time,
  # and this step has a long way to go after it.
  AS_SVC_TIMEOUT=600 as_svc sh -c "cd '$BUN_DIR' && exec npm install --no-audit --no-fund 'bun${want:+@$want}'" > "$BUN_LOG" 2>&1 </dev/null || return 1
  [ -x "$BUN_DIR/node_modules/.bin/bun" ] || return 1
  install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$BUN_DIR/bin"
  ln -sf "$BUN_DIR/node_modules/.bin/bun" "$BUN_DIR/bin/bun"
}

# install_bun_via_bunsh is bun's own installer, for the hosts with no npm.
install_bun_via_bunsh() {
  want="${1-}"
  ensure_pkg unzip

  # Deliberately NOT `curl … | bash`, which is how bun documents this and is
  # wrong for an unattended installer: a pipeline exits with the status of its
  # LAST command, so a curl that fails hands bash an empty script and bash
  # exits 0. The download failure vanishes and the only symptom is a missing
  # binary further on, which is a much worse thing to debug from. Fetch first,
  # check that, then run it.
  #
  # It lands in $BUN_DIR because the service user has to be able to read it,
  # and mktemp would give it a file owned by root and mode 600.
  installer="$BUN_DIR/install.sh"
  if ! curl -fsSL --connect-timeout 20 --max-time 300 https://bun.sh/install -o "$installer"; then
    rm -f "$installer"
    warn "could not fetch bun's installer from https://bun.sh/install."
    warn "Proton's client needs a bun. Check the network from this machine, or"
    warn "set PROTON_CLI_URL to a prebuilt proton-drive binary and re-run."
    return 1
  fi
  chown "$SVC_USER":"$SVC_USER" "$installer" 2>/dev/null || true
  chmod 0755 "$installer"

  # The output is kept rather than discarded. When this fails it is the only
  # thing that says why, and "it did not work" is not a bug report anybody can
  # act on — least of all somebody reading it over ssh on a Pi.
  # The installer takes a release to fetch as its argument, "bun-v1.4.2"; with
  # none it takes the latest, which is only right when nothing asked otherwise.
  if ! AS_SVC_TIMEOUT=600 as_svc env BUN_INSTALL="$BUN_DIR" bash "$installer" ${want:+"bun-v$want"} > "$BUN_LOG" 2>&1 </dev/null; then
    warn "bun's installer failed. Last lines of $BUN_LOG:"
    tail -n 6 "$BUN_LOG" 2>/dev/null | sed 's/^/       /' >&2
    return 1
  fi
}

# fetch_proton_sdk clones or updates the checkout the client is built from, and
# prints the commit it left it at, so the caller can tell a rebuild from a
# no-op.
fetch_proton_sdk() {
  # Output goes to a log rather than /dev/null for the same reason it does
  # everywhere else in this step: when a clone fails, git's own sentence is the
  # only thing that says whether it was the network, the ref or the disk.
  if [ -d "$PROTON_SDK_DIR/.git" ]; then
    AS_SVC_TIMEOUT=600 as_svc git -C "$PROTON_SDK_DIR" fetch --depth 1 origin "$PROTON_SDK_REF" > "$PROTON_FETCH_LOG" 2>&1 </dev/null || return 1
    as_svc git -C "$PROTON_SDK_DIR" checkout -q FETCH_HEAD >> "$PROTON_FETCH_LOG" 2>&1 </dev/null || return 1
  else
    rm -rf "$PROTON_SDK_DIR"
    install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$PROTON_SDK_DIR"
    AS_SVC_TIMEOUT=600 as_svc git clone --depth 1 --branch "$PROTON_SDK_REF" "$PROTON_SDK_REPO" "$PROTON_SDK_DIR" > "$PROTON_FETCH_LOG" 2>&1 </dev/null \
      || AS_SVC_TIMEOUT=600 as_svc git clone --depth 1 "$PROTON_SDK_REPO" "$PROTON_SDK_DIR" >> "$PROTON_FETCH_LOG" 2>&1 </dev/null || return 1
  fi
  as_svc git -C "$PROTON_SDK_DIR" rev-parse HEAD 2>/dev/null
}

# proton_bun_wanted reads the bun release Proton's client builds with out of
# cli/package.json ("engines": {"bun": "1.4.2"}). Only an exact x.y.z is
# returned: a range would be honoured by the build script's own check, and
# bun's installer can only fetch an exact release anyway. Empty means no pin,
# and the latest will do.
proton_bun_wanted() {
  sed -n 's/^[[:space:]]*"bun":[[:space:]]*"\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)".*/\1/p' \
    "$PROTON_SDK_DIR/cli/package.json" 2>/dev/null | head -n 1
}

# build_proton_cli compiles the client. CLI_APP_VERSION_NAME is not decoration:
# Proton requires every third-party client to identify itself honestly in the
# x-pm-appversion header, in this shape, and forbids passing as a first-party
# app. It is baked in at build time, which is the whole reason this is built
# here rather than repackaged.
# PROTON_SDK_PACKAGES is every package that has to have its dependencies
# installed, in dependency order.
#
# There are three rather than one because the SDK is a monorepo with no
# workspace root — no package.json at the top of it — so `bun install` in cli/
# links ../client/js and ../incubating/account/js as file: dependencies and
# never installs THEIRS. The build then dies resolving ttag, @xmldom/xmldom and
# exifreader out of packages that were linked but never populated, which is
# exactly what "Proton's client would not build" was hiding.
PROTON_SDK_PACKAGES="client/js incubating/account/js cli"

# How long the compile may take, in minutes. Forty-five is generous for a
# laptop and about right for a Raspberry Pi 4 with the memory to finish at
# all; past it the build is far more likely swapping than working.
PROTON_BUILD_TIMEOUT="${PROTON_BUILD_TIMEOUT:-45}"

# How long one dependency install may take. Bun is quick when the registry
# answers, and no amount of waiting helps when it does not.
PROTON_INSTALL_TIMEOUT=900

# run_watched runs one slow command with its output in a log, a line every
# half minute saying it is still going and what it last printed, and a limit on
# how long it may take. Nothing here used to say anything between "building
# Proton's client (this takes a while)…" and the end, and nothing stopped a
# command that never ended, so a slow Pi and a stalled download looked the same
# — and both looked like the installer had hung. The command's status comes
# back; 124 means it was stopped at the limit.
#
#   run_watched "label" seconds logfile command args…
run_watched() {
  label="$1"
  limit_s="$2"
  logfile="$3"
  shift 3

  AS_SVC_TIMEOUT="$limit_s" "$@" > "$logfile" 2>&1 </dev/null &
  pid=$!
  started="$(date +%s)"
  spoken=0
  while kill -0 "$pid" 2>/dev/null; do
    sleep 5
    elapsed=$(( $(date +%s) - started ))
    if [ $((elapsed / 30)) -gt "$spoken" ]; then
      spoken=$((elapsed / 30))
      last="$(tail -n 1 "$logfile" 2>/dev/null | tr -d '\r' | cut -c1-72)"
      log "  still ${label}… $((elapsed / 60))m$((elapsed % 60))s${last:+  ·  $last}"
    fi
  done
  wait "$pid"
}

build_proton_cli() {
  for package in $PROTON_SDK_PACKAGES; do
    log "installing dependencies in $package…"

    # --frozen-lockfile first, because a build of somebody else's tree should
    # use the versions they pinned. It is not worth failing the install over,
    # though: this tracks a moving ref, and a lockfile that needs migrating to
    # the bun just installed is a bad reason to have no Proton backend. So a
    # second pass without it, saying so.
    if run_watched "installing $package" "$PROTON_INSTALL_TIMEOUT" "$PROTON_BUILD_LOG" \
         proton_bun "$package" install --frozen-lockfile; then
      continue
    fi
    warn "the pinned install failed in $package; retrying without --frozen-lockfile."
    status=0
    run_watched "installing $package" "$PROTON_INSTALL_TIMEOUT" "$PROTON_BUILD_LOG" \
      proton_bun "$package" install || status=$?
    if [ "$status" -ne 0 ]; then
      if [ "$status" -eq 124 ]; then
        warn "installing dependencies in $package took longer than $((PROTON_INSTALL_TIMEOUT / 60)) minutes and was stopped."
        warn "The registry is slow or unreachable from this machine. Last lines of $PROTON_BUILD_LOG:"
      else
        warn "bun could not install dependencies in $package. Last lines of $PROTON_BUILD_LOG:"
      fi
      proton_log_tail "$PROTON_BUILD_LOG"
      return 1
    fi
  done

  status=0
  run_watched "building Proton's client" "$((PROTON_BUILD_TIMEOUT * 60))" "$PROTON_BUILD_LOG" \
    proton_bun cli run build || status=$?
  if [ "$status" -ne 0 ]; then
    if [ "$status" -eq 124 ]; then
      warn "the Proton client did not finish compiling in $PROTON_BUILD_TIMEOUT minutes and was stopped."
      warn "Raise PROTON_BUILD_TIMEOUT if this machine is simply slow. Last lines of $PROTON_BUILD_LOG:"
    else
      warn "the Proton client would not compile. Last lines of $PROTON_BUILD_LOG:"
    fi
    proton_log_tail "$PROTON_BUILD_LOG"
    proton_note_memory
    return 1
  fi
}

# proton_bun runs one bun command in one package of the SDK, with the
# environment the build needs. CLI_APP_VERSION_NAME is not decoration: Proton
# requires a third-party client to identify itself honestly in x-pm-appversion,
# and it is baked in at build time — the built binary reports
# external-drive-sand@<version>.
proton_bun() {
  package="$1"
  shift
  as_svc env \
      BUN_INSTALL="$BUN_DIR" \
      PATH="$BUN_DIR/bin:$PATH" \
      CLI_APP_VERSION_NAME="external-drive-sand" \
      sh -c "cd '$PROTON_SDK_DIR/$package' && exec bun $*"
}

proton_log_tail() {
  tail -n 12 "$1" 2>/dev/null | sed 's/^/       /' >&2
}

# proton_note_memory adds the sentence the log usually will not. A compile
# killed for memory says little or nothing — the kernel takes the process out
# and bun never gets to complain — and on a 2 GB Pi that is the first thing to
# suspect rather than the last.
proton_note_memory() {
  total_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  [ "${total_kb:-0}" -gt 0 ] 2>/dev/null || return 0
  [ "$total_kb" -lt 3000000 ] || return 0
  warn "This machine has $((total_kb / 1024)) MB of RAM. Compiling the client needs"
  warn "more than a small board usually has, and a build killed for memory often"
  warn "says nothing at all. PROTON_CLI_URL takes a prebuilt binary instead:"
  warn "  https://proton.me/download/drive/cli/index.html"
}

install_proton_cli() {
  install -d -o "$SVC_USER" -g "$SVC_USER" -m 755 "$PREFIX/bin"

  # A prebuilt binary, when one has been named. This is the fast path and the
  # only practical one on a small board.
  if [ -n "$PROTON_CLI_URL" ]; then
    log "downloading Proton's client…"
    tmp="$(mktemp)"
    if ! curl -fsSL --connect-timeout 20 --max-time 900 "$PROTON_CLI_URL" -o "$tmp"; then
      rm -f "$tmp"
      warn "could not download $PROTON_CLI_URL — Proton accounts will need the synced folder."
      return 1
    fi
    install -m 755 -o "$SVC_USER" -g "$SVC_USER" "$tmp" "$PROTON_BIN"
    rm -f "$tmp"
    return 0
  fi

  if ! bun_supported; then
    warn "bun has no build for $(bun_arch), so Proton's client cannot be built here."
    warn "Connect Proton as a synced folder, or set PROTON_CLI_URL to a prebuilt binary."
    return 1
  fi

  log "fetching Proton's Drive SDK…"
  if ! rev="$(fetch_proton_sdk)"; then
    warn "could not fetch $PROTON_SDK_REPO. Last lines of $PROTON_FETCH_LOG:"
    proton_log_tail "$PROTON_FETCH_LOG"
    return 1
  fi

  # Rebuilding takes minutes and produces the same binary from the same commit,
  # so an upgrade that has not moved the SDK skips it — before fetching a bun,
  # which it would not need either.
  if [ -x "$PROTON_BIN" ] && [ "$(cat "$PROTON_STAMP" 2>/dev/null)" = "$rev" ]; then
    ok "Proton client already built from $(printf '%.7s' "$rev")"
    return 0
  fi

  # The SDK first, the bun second: the SDK says which bun it builds with.
  install_bun "$(proton_bun_wanted)" || return 1

  log "building Proton's client (this takes a while)…"
  if ! build_proton_cli; then
    # build_proton_cli has already said what failed and why. This is the part
    # somebody needs next: the install is fine, and Proton has another route.
    warn "The rest of the install is unaffected. Connect Proton as a synced"
    warn "folder, or set PROTON_CLI_URL to a prebuilt binary and re-run."
    return 1
  fi
  [ -f "$PROTON_SDK_DIR/cli/release/proton-drive" ] || {
    warn "the Proton build finished but produced no binary."
    return 1
  }
  install -m 755 -o "$SVC_USER" -g "$SVC_USER" "$PROTON_SDK_DIR/cli/release/proton-drive" "$PROTON_BIN"
  printf '%s' "$rev" > "$PROTON_STAMP"
}

PROTON_READY=0
if [ "$INSTALL_PROTON" = never ]; then
  ok "skipping Proton's client (INSTALL_PROTON=never)"
elif install_proton_cli; then
  # On PATH under its own name, so the service finds it without being told and
  # `sand remote proton login` works from an ordinary shell.
  ln -sf "$PROTON_BIN" "$PROTON_LINK"
  PROTON_READY=1
  ok "proton-drive installed → $PROTON_BIN"
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

$(if [ "$PROTON_READY" = 1 ]; then cat <<PROTON

  Proton Drive: sign in once, in a browser on any device —
    $SERVER_BIN --vault $VAULT_PATH remote proton login --name proton
  It prints a link and waits. Nothing is typed here and no password reaches SAND.
PROTON
fi)
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
