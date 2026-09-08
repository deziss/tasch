#!/bin/bash
set -e

# Create tasch user if it doesn't exist
if ! getent passwd tasch > /dev/null; then
    useradd --system --user-group --home-dir /var/lib/tasch --create-home --shell /bin/false tasch
fi

# Ensure /var/lib/tasch exists and has correct permissions
mkdir -p /var/lib/tasch
chown -R tasch:tasch /var/lib/tasch
chmod 750 /var/lib/tasch

# Ensure /etc/tasch exists.
#
# 0750 and root-owned: the config names TLS key paths and can hold authentication tokens, and
# the daemon only needs to read it. Leaving it writable by the tasch user meant anyone who
# reached code execution as that account could rewrite its own configuration.
mkdir -p /etc/tasch
chown root:tasch /etc/tasch
chmod 750 /etc/tasch
if [ -f /etc/tasch/config.yaml ]; then
    chown root:tasch /etc/tasch/config.yaml
    chmod 640 /etc/tasch/config.yaml
fi

# Reload systemd if available
if command -v systemctl > /dev/null; then
    systemctl daemon-reload
fi
