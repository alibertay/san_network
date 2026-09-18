#!/usr/bin/env bash
# SAN Network node installer for Linux (systemd).
#
# Idempotent: safe to re-run for upgrades. Builds the Go binaries for the host
# architecture, installs them under --prefix, creates the "san" system user and
# the data/config directories, optionally generates a devnet CA/node certificate
# and writes /etc/san/san.env, then installs and enables the systemd unit.
#
#   sudo deploy/install.sh
#   sudo deploy/install.sh --advertise-host node1.example.com
#   sudo deploy/install.sh --prefix /opt/san --data-dir /srv/san
#   sudo deploy/install.sh --no-cert --no-systemd
#   sudo deploy/install.sh --uninstall            # keep data and config
#   sudo deploy/install.sh --uninstall --purge    # remove data and config too
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

PREFIX=/usr/local
DATA_DIR=/var/lib/san
CONFIG_DIR=/etc/san
SAN_USER=san
SAN_GROUP=san
ADVERTISE_HOST=
WITH_CERT=1
WITH_SYSTEMD=1
UNINSTALL=0
PURGE=0
GO_BIN=${GO:-}

log() { printf 'install.sh: %s\n' "$*"; }
die() { printf 'install.sh: error: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
usage: deploy/install.sh [options]

  --prefix DIR        install prefix (default /usr/local)
  --data-dir DIR      node data directory (default /var/lib/san)
  --config-dir DIR    configuration directory (default /etc/san)
  --user NAME         system user/group (default san)
  --advertise-host H  public DNS name/IP for the node certificate (SANs)
  --go PATH           go binary (default: go from PATH or /usr/local/go/bin/go)
  --no-cert           do not generate a devnet CA/node certificate
  --no-systemd        do not install/enable the systemd unit
  --uninstall         stop and remove the installed service and binaries
  --purge             with --uninstall: also remove data and configuration
  -h, --help          this help
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix) PREFIX=$2; shift 2 ;;
        --data-dir) DATA_DIR=$2; shift 2 ;;
        --config-dir) CONFIG_DIR=$2; shift 2 ;;
        --user) SAN_USER=$2; SAN_GROUP=$2; shift 2 ;;
        --advertise-host) ADVERTISE_HOST=$2; shift 2 ;;
        --go) GO_BIN=$2; shift 2 ;;
        --no-cert) WITH_CERT=0; shift ;;
        --no-systemd) WITH_SYSTEMD=0; shift ;;
        --uninstall) UNINSTALL=1; shift ;;
        --purge) PURGE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option $1 (try --help)" ;;
    esac
done

BIN_DIR=$PREFIX/bin
CERTS_DIR=$CONFIG_DIR/certs
ENV_FILE=$CONFIG_DIR/san.env
UNIT_FILE=/etc/systemd/system/san-node.service

# The advertised host is used for the certificate SANs and the environment
# template; fall back to the machine's FQDN.
if [ -z "$ADVERTISE_HOST" ]; then
    ADVERTISE_HOST=$(hostname -f 2>/dev/null || hostname)
fi

need_root() {
    [ "$(id -u)" = "0" ] || die "this action needs root (run with sudo)"
}

find_go() {
    if [ -n "$GO_BIN" ]; then
        command -v "$GO_BIN" >/dev/null 2>&1 || die "go binary not found: $GO_BIN"
        return
    fi
    if command -v go >/dev/null 2>&1; then
        GO_BIN=$(command -v go)
    elif [ -x /usr/local/go/bin/go ]; then
        GO_BIN=/usr/local/go/bin/go
    else
        die "go not found; install Go 1.26+ or pass --go /path/to/go"
    fi
}

host_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        armv7l|armv7) echo arm ;;
        *) uname -m ;;
    esac
}

systemctl_available() {
    command -v systemctl >/dev/null 2>&1
}

maybe_create_user() {
    if id "$SAN_USER" >/dev/null 2>&1; then
        return
    fi
    if ! getent group "$SAN_GROUP" >/dev/null 2>&1 && command -v groupadd >/dev/null 2>&1; then
        groupadd --system "$SAN_GROUP" 2>/dev/null || true
    fi
    if command -v useradd >/dev/null 2>&1; then
        if getent group "$SAN_GROUP" >/dev/null 2>&1; then
            useradd --system --gid "$SAN_GROUP" --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SAN_USER"
        else
            useradd --system --user-group --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SAN_USER"
        fi
    elif command -v adduser >/dev/null 2>&1; then
        adduser --system --home "$DATA_DIR" --shell /usr/sbin/nologin --group "$SAN_USER"
    else
        log "warning: cannot create user $SAN_USER (no useradd/adduser); using current user"
    fi
}

service_owner_user() {
    if id "$SAN_USER" >/dev/null 2>&1; then
        printf '%s' "$SAN_USER"
    else
        id -un
    fi
}

service_owner_group() {
    if id "$SAN_USER" >/dev/null 2>&1; then
        id -gn "$SAN_USER"
    else
        id -gn
    fi
}

install_unit() {
    sed -e "s|@PREFIX@|$PREFIX|g" \
        -e "s|@DATA_DIR@|$DATA_DIR|g" \
        -e "s|@CONFIG_DIR@|$CONFIG_DIR|g" \
        -e "s|@USER@|$SAN_USER|g" \
        -e "s|@GROUP@|$SAN_GROUP|g" \
        "$SCRIPT_DIR/san-node.service" > "$UNIT_FILE"
    chmod 0644 "$UNIT_FILE"
    if [ "$WITH_SYSTEMD" = "1" ] && systemctl_available; then
        systemctl daemon-reload || true
        systemctl enable san-node.service >/dev/null 2>&1 || true
        # Restart an already-running service so a re-run upgrades the binaries.
        if systemctl is-active --quiet san-node.service; then
            action=restarted
        else
            action=started
        fi
        if systemctl restart san-node.service 2>/dev/null || systemctl start san-node.service 2>/dev/null; then
            log "systemd unit san-node.service enabled and $action"
        else
            log "systemd unit installed; start it with: systemctl start san-node"
        fi
    else
        log "systemd unit written to $UNIT_FILE (not enabled)"
        log "start it with: sudo systemctl daemon-reload && sudo systemctl enable --now san-node"
    fi
    log "service owner: $(service_owner_user):$(service_owner_group)"
}

do_uninstall() {
    need_root
    if systemctl_available; then
        systemctl stop san-node.service 2>/dev/null || true
        systemctl disable san-node.service 2>/dev/null || true
    fi
    rm -f "$UNIT_FILE"
    if systemctl_available; then
        systemctl daemon-reload || true
    fi
    rm -f "$BIN_DIR/sannode" "$BIN_DIR/sanup" "$BIN_DIR/sancli"
    log "removed binaries and the systemd unit"
    if [ "$PURGE" = "1" ]; then
        rm -rf "$DATA_DIR" "$CONFIG_DIR"
        if command -v userdel >/dev/null 2>&1 && id "$SAN_USER" >/dev/null 2>&1; then
            userdel "$SAN_USER" 2>/dev/null || true
        fi
        log "purged data, configuration and the $SAN_USER user"
    else
        log "kept $DATA_DIR and $CONFIG_DIR (use --purge to remove them)"
    fi
}

if [ "$UNINSTALL" = "1" ]; then
    do_uninstall
    exit 0
fi

need_root
find_go

command -v gcc >/dev/null 2>&1 && HAVE_GCC=1 || HAVE_GCC=0
GO_TAGS=
CGO=0
if [ "$HAVE_GCC" = "1" ]; then
    # The bundled LMDB only needs a C compiler; this enables real persistence.
    CGO=1
    GO_TAGS="-tags lmdb"
else
    log "warning: gcc not found; building without LMDB (the node uses the in-memory backend)"
fi

log "building sannode, sanup and sancli for $("$GO_BIN" env GOOS)/$("$GO_BIN" env GOARCH) (cgo=$CGO)"
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT INT TERM
(
    cd "$ROOT_DIR"
    CGO_ENABLED=$CGO "$GO_BIN" build -buildvcs=false $GO_TAGS -trimpath -ldflags="-s -w" -o "$BUILD_DIR/sannode" ./cmd/sannode
    CGO_ENABLED=$CGO "$GO_BIN" build -buildvcs=false $GO_TAGS -trimpath -ldflags="-s -w" -o "$BUILD_DIR/sanup" ./cmd/sanup
    CGO_ENABLED=$CGO "$GO_BIN" build -buildvcs=false $GO_TAGS -trimpath -ldflags="-s -w" -o "$BUILD_DIR/sancli" ./cmd/sancli
)

maybe_create_user
OWNER_USER=$(service_owner_user)
OWNER_GROUP=$(service_owner_group)

install -d -m 0755 "$BIN_DIR"
install -m 0755 "$BUILD_DIR/sannode" "$BIN_DIR/sannode"
install -m 0755 "$BUILD_DIR/sanup" "$BIN_DIR/sanup"
install -m 0755 "$BUILD_DIR/sancli" "$BIN_DIR/sancli"
log "installed binaries to $BIN_DIR (arch $(host_arch))"

install -d -m 0700 -o "$OWNER_USER" -g "$OWNER_GROUP" "$DATA_DIR"
install -d -m 0750 "$CONFIG_DIR"
# The service group must be able to traverse /etc/san to read the certificates
# and the environment file.
chown "root:$OWNER_GROUP" "$CONFIG_DIR" 2>/dev/null || true

if [ "$WITH_CERT" = "1" ] && [ ! -f "$CERTS_DIR/node.crt" ]; then
    install -d -m 0700 -o "$OWNER_USER" -g "$OWNER_GROUP" "$CERTS_DIR"
    "$BIN_DIR/sanup" cert --dir "$CERTS_DIR" --advertise-host "$ADVERTISE_HOST" >/dev/null
    chown "$OWNER_USER:$OWNER_GROUP" "$CERTS_DIR/node.crt" "$CERTS_DIR/node.key" 2>/dev/null || true
    chmod 0644 "$CERTS_DIR/ca.crt" 2>/dev/null || true
    chmod 0600 "$CERTS_DIR/ca.key" "$CERTS_DIR/node.key" 2>/dev/null || true
    log "generated devnet CA/node certificates in $CERTS_DIR (node.key 0600)"
    log "copy $CERTS_DIR/ca.crt to every peer and reference it in $ENV_FILE"
else
    log "certificates already present or --no-cert; leaving $CERTS_DIR untouched"
fi

if [ ! -f "$ENV_FILE" ]; then
    sed -e "s|@DATA_DIR@|$DATA_DIR|g" \
        -e "s|@CONFIG_DIR@|$CONFIG_DIR|g" \
        -e "s|@ADVERTISE_HOST@|$ADVERTISE_HOST|g" \
        "$SCRIPT_DIR/san.env.example" > "$ENV_FILE"
    chmod 0640 "$ENV_FILE"
    chown "root:$SAN_GROUP" "$ENV_FILE" 2>/dev/null || true
    log "wrote $ENV_FILE (edit it before exposing the node)"
else
    log "$ENV_FILE already exists; left unchanged"
fi

if [ "$WITH_CERT" = "1" ] && [ -f "$CERTS_DIR/node.crt" ]; then
    if ! grep -q '^SAN_TLS_CERT=' "$ENV_FILE" 2>/dev/null; then
        printf '\nSAN_TLS_CERT=%s/node.crt\nSAN_TLS_KEY=%s/node.key\nSAN_TLS_CA=%s/ca.crt\n' \
            "$CERTS_DIR" "$CERTS_DIR" "$CERTS_DIR" >> "$ENV_FILE"
        log "enabled TLS in $ENV_FILE (node.crt/node.key/ca.crt)"
    fi
fi

if [ "$WITH_SYSTEMD" = "1" ]; then
    install_unit
else
    log "systemd unit not installed (--no-systemd)"
    log "run the node with: sudo -u $SAN_USER $BIN_DIR/sanup --foreground"
fi
