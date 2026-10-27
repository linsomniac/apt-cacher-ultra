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
# package operation: systemctl's error is printed, journalctl -u
# apt-cacher-ultra says why, and the unit's Restart=always keeps trying.
# A reinstall after a remove (not a purge) also passes $2, as with
# debhelper: nothing is running to restart, and no message is printed,
# though the remove disabled the unit.
if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ]; then
    if [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
        systemctl --system daemon-reload >/dev/null 2>&1 || :
        if [ -n "$2" ]; then
            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
        fi
    fi
fi

if [ "$1" = "configure" ] && [ -z "$2" ]; then
    cat <<'EOF'
apt-cacher-ultra installed.
Edit /etc/apt-cacher-ultra/config.toml, then:
  sudo systemctl enable --now apt-cacher-ultra
EOF
fi
