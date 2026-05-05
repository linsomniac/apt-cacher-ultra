#!/bin/sh
set -e

# Debian prerm is invoked with $1 in {remove, upgrade, deconfigure,
# failed-upgrade}. Only stop and disable on outright remove; on upgrade,
# the new package's postinst will restart the service, and disabling here
# would leave it disabled afterwards.
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
    systemctl stop apt-cacher-ultra.service >/dev/null 2>&1 || :
    systemctl disable apt-cacher-ultra.service >/dev/null 2>&1 || :
fi
