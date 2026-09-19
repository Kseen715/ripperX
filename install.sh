#!/bin/sh
# Install ripperX: build the binary, put xorriso beside it so discs can be
# written, and run it from boot as a systemd service that owns nothing but
# the drives and its own directory.
#
# Run from the repository root:   sudo ./install.sh
#   --no-service   install the binary only, and start it by hand
#   --user NAME    run as an existing user instead of a dedicated one
#   --addr ADDR    what to listen on; the default is this machine only
set -eu

BIN_DIR="${BIN_DIR:-/usr/local/bin}"
UNIT="/etc/systemd/system/ripperx.service"
CONF="/etc/ripperx.conf"
STATE="/var/lib/ripperx"
SERVICE_USER="ripperx"

WANT_SERVICE=yes
# Localhost by default. ripperX starts with no password set, and a service
# that hands out the drives of a machine should not appear on the network
# because somebody ran an installer.
ADDR="127.0.0.1:8080"

while [ $# -gt 0 ]; do
    case "$1" in
        --no-service) WANT_SERVICE=no ;;
        --user) shift; [ $# -gt 0 ] || { printf 'error: --user needs a name\n' >&2; exit 1; }
                SERVICE_USER="$1" ;;
        --addr) shift; [ $# -gt 0 ] || { printf 'error: --addr needs an address\n' >&2; exit 1; }
                ADDR="$1" ;;
        -h|--help)
            printf 'usage: sudo ./install.sh [--no-service] [--user NAME] [--addr ADDR]\n'
            printf '  --no-service  install the binary only\n'
            printf '  --user NAME   run as an existing user rather than a dedicated one\n'
            printf '  --addr ADDR   listen address for a fresh settings file (default %s)\n' "$ADDR"
            exit 0 ;;
        *) printf 'error: unknown option %s\n' "$1" >&2; exit 1 ;;
    esac
    shift
done

say()  { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
warn() { printf 'note: %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this with sudo (it installs a binary and a systemd unit)"

step "Detecting package manager"
if   command -v apt-get      >/dev/null 2>&1; then PM=apt
elif command -v xbps-install >/dev/null 2>&1; then PM=xbps
elif command -v pacman       >/dev/null 2>&1; then PM=pacman
elif command -v dnf          >/dev/null 2>&1; then PM=dnf
else PM=none
fi
say "using: $PM"

install_pkgs() {
    case "$PM" in
        apt)    DEBIAN_FRONTEND=noninteractive apt-get update -qq
                DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
        xbps)   xbps-install -Sy "$@" ;;
        pacman) pacman -Sy --needed --noconfirm "$@" ;;
        dnf)    dnf install -y "$@" ;;
        none)   return 1 ;;
    esac
}

# Inside a .run installer the binary is already built and sits next to this
# script, so the whole toolchain half is skipped. Everything after it is
# identical either way.
# An if, not a && chain: under `set -e` a false chain would end the script.
PREBUILT=""
if [ -x "./ripperx" ] && [ ! -f go.mod ]; then
    PREBUILT="./ripperx"
fi

if [ -n "$PREBUILT" ]; then
    step "Installing the bundled ripperx binary"
    install -m 0755 "$PREBUILT" "${BIN_DIR}/ripperx"
    say "installed ${BIN_DIR}/ripperx (prebuilt, no toolchain needed)"
else
    step "Build dependencies"
    # Go is needed only to build. The finished binary has no runtime
    # dependencies at all: it talks to the drives through an ioctl, and to
    # the share over the wire.
    if command -v go >/dev/null 2>&1; then
        say "go already present: $(go version)"
    else
        case "$PM" in
            apt)    install_pkgs golang-go ;;
            xbps)   install_pkgs go ;;
            pacman) install_pkgs go ;;
            dnf)    install_pkgs golang ;;
            none)   die "no supported package manager found; install Go manually, then re-run" ;;
        esac
    fi
    command -v go >/dev/null 2>&1 || die "Go is still not on PATH; install it and re-run"

    step "Building ripperx"
    [ -f go.mod ] || die "run this from the repository root (go.mod not found)"
    # Keep the build cache inside the tree so root does not scribble in
    # ~/.cache. -buildvcs=false: running under sudo in a repository owned by
    # another user makes git refuse to report status, which fails the build.
    GOCACHE="${PWD}/.gocache" go build -trimpath -buildvcs=false \
        -ldflags '-s -w' -o "${BIN_DIR}/ripperx" ./cmd/ripperx
    say "installed ${BIN_DIR}/ripperx"
fi

step "The burner"
# Writing a disc is handed to xorriso: getting MODE SELECT, the write
# parameters page and the close sequence right for a particular drive's
# firmware is decades of accumulated knowledge, and getting it wrong costs a
# disc. Without it ripperX reads discs and refuses to write them, which is a
# working install rather than a broken one.
if command -v xorriso >/dev/null 2>&1; then
    say "xorriso already present: $(command -v xorriso)"
elif install_pkgs xorriso 2>/dev/null && command -v xorriso >/dev/null 2>&1; then
    say "installed xorriso"
else
    warn "xorriso is not installed, so discs can be read but not written."
    warn "install it later and restart ripperX; nothing else has to change."
fi

# The group that owns the drive nodes differs by distribution, and getting it
# wrong is an install that reads no discs at all. Ask the nodes themselves,
# and fall back to whichever group exists.
step "Finding the group that owns the drives"
DISC_GROUP=""
for node in /dev/sr0 /dev/sr1 /dev/sg0; do
    [ -e "$node" ] || continue
    g="$(stat -c '%G' "$node" 2>/dev/null || true)"
    case "$g" in ""|root|UNKNOWN) ;; *) DISC_GROUP="$g"; break ;; esac
done
if [ -z "$DISC_GROUP" ]; then
    for g in cdrom optical; do
        if getent group "$g" >/dev/null 2>&1; then DISC_GROUP="$g"; break; fi
    done
fi
[ -n "$DISC_GROUP" ] || DISC_GROUP=cdrom
say "disc group: $DISC_GROUP"
getent group "$DISC_GROUP" >/dev/null 2>&1 || {
    groupadd --system "$DISC_GROUP"
    say "created the group $DISC_GROUP"
}

if [ "$WANT_SERVICE" = no ]; then
    step "Done"
    say "Start it with:   sudo ripperx --addr ${ADDR}      then open http://${ADDR}/"
    say "To run it from boot instead:  sudo ./install.sh"
    exit 0
fi

step "Installing the systemd service"
command -v systemctl >/dev/null 2>&1 || die "the service needs systemd, which is not present here"

# A dedicated system user unless one was named. It owns the images and the
# history and nothing else: a service that hands out the contents of discs
# should not be able to read the rest of the machine.
if id "$SERVICE_USER" >/dev/null 2>&1; then
    say "running as the existing user $SERVICE_USER"
else
    useradd --system --home-dir "$STATE" --create-home \
        --shell /usr/sbin/nologin --user-group "$SERVICE_USER" 2>/dev/null ||
        useradd --system --home-dir "$STATE" --create-home \
            --shell /sbin/nologin --user-group "$SERVICE_USER" ||
        die "could not create the user $SERVICE_USER"
    say "created the system user $SERVICE_USER"
fi
# Reading a disc needs membership of the group that owns the node; the
# capability in the unit covers writing one.
usermod -aG "$DISC_GROUP" "$SERVICE_USER"
say "added $SERVICE_USER to $DISC_GROUP"

install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$STATE"
# Recursive, because an earlier run as root leaves a history database in
# there that the service user then cannot open. This directory holds only
# ripperX's own images and history, so there is nothing else in it to take.
chown -R "$SERVICE_USER":"$SERVICE_USER" "$STATE"

# The settings file is the one place a password lives, so it is never
# overwritten and never made readable by anyone else. ripperX refuses to
# start if it holds a secret and is mode other than 600.
if [ -e "$CONF" ]; then
    say "$CONF already exists, leaving it alone; ripperX takes its settings from there"
    chown "$SERVICE_USER":"$SERVICE_USER" "$CONF"
    chmod 0600 "$CONF"
else
    [ -f examples/ripperx.conf ] || die "examples/ripperx.conf not found"
    install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 examples/ripperx.conf "$CONF"
    # The settings go in the file rather than into the unit, so changing one
    # later is an edit here and a restart, with nothing to override it.
    {
        printf '\n# Written by install.sh.\n'
        printf 'addr = %s\n' "$ADDR"
        printf 'out = %s\n' "$STATE"
        printf 'history = %s/ripperx.db\n' "$STATE"
    } >> "$CONF"
    say "wrote $CONF (mode 0600, owned by $SERVICE_USER)"
fi

# The unit is rendered from the committed example, so the file on disk and
# the one in examples/ never drift apart.
[ -f examples/systemd/ripperx.service ] || die "examples/systemd/ripperx.service not found"
sed -e "s/%USER%/${SERVICE_USER}/g" \
    -e "s/%DISCGROUP%/${DISC_GROUP}/g" \
    -e "s#/usr/local/bin/ripperx#${BIN_DIR}/ripperx#g" \
    examples/systemd/ripperx.service > "$UNIT"
chmod 0644 "$UNIT"

systemctl daemon-reload
systemctl enable --now ripperx

step "Done"
say "ripperX runs as $SERVICE_USER, keeping images in $STATE"
say "  open:    http://${ADDR}/"
say "  status:  systemctl status ripperx      logs: journalctl -u ripperx -f"
say "  settings: $CONF   (edit, then: sudo systemctl restart ripperx)"
say "  stop it: sudo systemctl disable --now ripperx"
say ""
say "Two settings worth knowing about, both in $CONF:"
say "  * auth-user and auth-password turn on the login. Until they are set,"
say "    anyone who can reach the address above can use the drives - which is"
say "    why the default address is this machine only."
say "  * smb-address points the image store at a share instead of $STATE,"
say "    so rips land on the file server rather than on this disk."

if ! systemctl is-active --quiet ripperx; then
    say ""
    warn "the service is not running. What it said:"
    systemctl status ripperx --no-pager --lines=10 || true
fi
