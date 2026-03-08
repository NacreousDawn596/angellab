#!/usr/bin/env bash
# scripts/install.sh — AngelLab system setup
# Creates system user, directories, and sets correct permissions.
# Must be run as root.

set -euo pipefail

ANGELLAB_USER="angellab"
ANGELLAB_GROUP="angellab"

RUN_DIR="/run/angellab"
VAR_DIR="/var/lib/angellab"
SNAP_DIR="/var/lib/angellab/snapshots"
LOG_DIR="/var/log/angellab"
CONF_DIR="/etc/angellab"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

info()  { echo "  [INFO]  $*"; }
ok()    { echo "  [ OK ]  $*"; }
warn()  { echo "  [WARN]  $*"; }

require_root() {
    if [[ $EUID -ne 0 ]]; then
        echo "error: install.sh must be run as root" >&2
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Create system user and group
# ---------------------------------------------------------------------------

create_user() {
    if id "$ANGELLAB_USER" &>/dev/null; then
        info "user '$ANGELLAB_USER' already exists"
    else
        info "creating system user '$ANGELLAB_USER'"
        useradd \
            --system \
            --no-create-home \
            --shell /usr/sbin/nologin \
            --comment "AngelLab daemon" \
            "$ANGELLAB_USER"
        ok "user '$ANGELLAB_USER' created"
    fi
}

# ---------------------------------------------------------------------------
# Create directories with correct ownership and permissions
# ---------------------------------------------------------------------------

create_dirs() {
    info "creating runtime directories"

    # /run/angellab — socket lives here; world-readable dir, socket is 0600
    install -dm755 -o root -g root "$RUN_DIR"

    # /var/lib/angellab — registry and state; angellab-owned
    install -dm750 -o "$ANGELLAB_USER" -g "$ANGELLAB_GROUP" "$VAR_DIR"

    # /var/lib/angellab/snapshots — snapshot blobs; private
    install -dm700 -o "$ANGELLAB_USER" -g "$ANGELLAB_GROUP" "$SNAP_DIR"

    # /var/log/angellab — log files; readable by angellab group
    install -dm750 -o "$ANGELLAB_USER" -g "$ANGELLAB_GROUP" "$LOG_DIR"

    # /etc/angellab — config; readable by root and angellab
    install -dm750 -o root -g "$ANGELLAB_GROUP" "$CONF_DIR"

    ok "directories created"
}

# ---------------------------------------------------------------------------
# Install default config if not present
# ---------------------------------------------------------------------------

install_config() {
    if [[ -f "$CONF_DIR/angellab.toml" ]]; then
        info "config already exists at $CONF_DIR/angellab.toml — skipping"
    else
        SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        if [[ -f "$SCRIPT_DIR/../configs/angellab.toml" ]]; then
            install -m640 -o root -g "$ANGELLAB_GROUP" \
                "$SCRIPT_DIR/../configs/angellab.toml" \
                "$CONF_DIR/angellab.toml"
            ok "default config installed to $CONF_DIR/angellab.toml"
        else
            warn "configs/angellab.toml not found — skipping"
        fi
    fi
}

# ---------------------------------------------------------------------------
# Create /run/angellab on each boot via tmpfiles.d
# ---------------------------------------------------------------------------

install_tmpfiles() {
    local tmpfiles_dir="/etc/tmpfiles.d"
    if [[ -d "$tmpfiles_dir" ]]; then
        cat > "$tmpfiles_dir/angellab.conf" <<'EOF'
# AngelLab runtime directory
# Type  Path              Mode  User       Group      Age
d       /run/angellab     0755  root       root       -
EOF
        ok "tmpfiles.d entry written"
    else
        warn "/etc/tmpfiles.d not found — skipping tmpfiles.d setup"
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    require_root
    echo ""
    echo "AngelLab System Installer"
    echo "========================="
    echo ""

    create_user
    create_dirs
    install_config
    install_tmpfiles

    echo ""
    echo "Installation complete."
    echo ""
    echo "  Start service:  systemctl start angellab"
    echo "  Check status:   lab angel list"
    echo "  Follow events:  lab events"
    echo ""
}

main "$@"
