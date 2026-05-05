#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl stop apt-cacher-ultra.service >/dev/null 2>&1 || :
    systemctl disable apt-cacher-ultra.service >/dev/null 2>&1 || :
fi
