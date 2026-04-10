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

# Ensure /etc/tasch exists
mkdir -p /etc/tasch
chown tasch:tasch /etc/tasch
chmod 755 /etc/tasch

# Reload systemd if available
if command -v systemctl > /dev/null; then
    systemctl daemon-reload
fi
