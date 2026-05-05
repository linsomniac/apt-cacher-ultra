#!/bin/sh
set -e

if ! getent passwd apt-cacher-ultra >/dev/null 2>&1; then
    adduser --system --group --no-create-home \
        --home /var/cache/apt-cacher-ultra \
        --gecos "apt-cacher-ultra cache daemon" \
        apt-cacher-ultra
fi
