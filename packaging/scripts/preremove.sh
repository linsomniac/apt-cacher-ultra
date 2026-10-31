#!/bin/sh
set -e

# Debian prerm is invoked with $1 in {remove, upgrade, deconfigure,
# failed-upgrade}. Only stop and disable on outright remove. On upgrade
# this (the old package's) prerm stops nothing, so the daemon keeps
# running the old binary until the new package's postinst restarts it
# (try-restart: only if it is running); disabling here would leave it
# disabled afterwards.
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
    systemctl stop apt-cacher-ultra.service >/dev/null 2>&1 || :
    systemctl disable apt-cacher-ultra.service >/dev/null 2>&1 || :
fi
