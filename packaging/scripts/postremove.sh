#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
fi

# On purge: drop the user but preserve /var/cache/apt-cacher-ultra so cached
# content survives reinstall.
if [ "$1" = "purge" ]; then
    if getent passwd apt-cacher-ultra >/dev/null 2>&1; then
        deluser --system apt-cacher-ultra >/dev/null 2>&1 || :
    fi
fi
