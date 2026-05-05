#!/bin/sh
set -e

mkdir -p /var/cache/apt-cacher-ultra
chown apt-cacher-ultra:apt-cacher-ultra /var/cache/apt-cacher-ultra
chmod 0750 /var/cache/apt-cacher-ultra

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
fi

cat <<'EOF'
apt-cacher-ultra installed.
Edit /etc/apt-cacher-ultra/config.toml, then:
  sudo systemctl enable --now apt-cacher-ultra
EOF
