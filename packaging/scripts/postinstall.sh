#!/bin/sh
set -e

mkdir -p /var/cache/apt-cacher-ultra
chown apt-cacher-ultra:apt-cacher-ultra /var/cache/apt-cacher-ultra
chmod 0750 /var/cache/apt-cacher-ultra

# AIDEV-NOTE: what `dh_installsystemd --no-start --restart-after-upgrade`
# generates (debhelper's postinst-systemd-restartnostart snippet), written
# out because nfpm has no debhelper. On an upgrade (configure with $2, the
# version configured before) and on the abort-* actions, a running daemon
# is restarted onto this package's binary; a stopped one stays stopped
# (try-restart), and a fresh install starts nothing (the operator enables
# the unit). The old package's prerm stops nothing on upgrade
# (preremove.sh), so the daemon runs until this restart. deb-systemd-invoke
# rather than systemctl: it honours /usr/sbin/policy-rc.d (exit 101 forbids
# service actions, as in container builds and chroots), then passes
# try-restart to systemctl as it is, so a running unit is restarted whether
# or not it is enabled. It ships in init-system-helpers, Essential: yes on
# every supported release (Ubuntu 22.04-26.04, Debian 11-13), so the
# package needs no dependency for it. A failed restart never fails the
# package operation. systemctl reports only a failed restart job (the start
# rate limit, say); a new binary that exits at startup is not seen here
# (Type=simple: the job is done once the process is forked), only in
# `systemctl status` / `journalctl -u apt-cacher-ultra`, and the unit's
# Restart=always retries it every 5 s. A reinstall after a remove (not a
# purge) also passes $2, as with debhelper: the remove stopped and disabled
# the unit, so there is nothing to restart, and the install hint below
# prints again.
if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ]; then
    if [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
        systemctl --system daemon-reload >/dev/null 2>&1 || :
        if [ -n "$2" ]; then
            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
        fi
    fi
fi

# unit_enabled: true only where systemd runs (the guard above) and reports
# the unit enabled. Called only as an if condition, so a systemctl that
# answers "disabled" or "not-found" (exit 1) cannot trip set -e.
unit_enabled() {
    [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ] &&
        systemctl is-enabled --quiet apt-cacher-ultra.service >/dev/null 2>&1
}

# AIDEV-NOTE: the install hint prints on configure unless the unit is
# enabled: on a fresh install, and on a reinstall after a remove (whose
# prerm disabled the unit; 1.0.x printed the hint on every configure), but
# not on the usual upgrade of an enabled unit. Without systemd (a container,
# a chroot, DPKG_ROOT) systemctl is not asked, so the hint prints, as
# 1.0.x's did. Not on the abort-* actions: they install nothing.
if [ "$1" = "configure" ] && ! unit_enabled; then
    cat <<'EOF'
apt-cacher-ultra installed.
Edit /etc/apt-cacher-ultra/config.toml, then:
  sudo systemctl enable --now apt-cacher-ultra
EOF
fi
