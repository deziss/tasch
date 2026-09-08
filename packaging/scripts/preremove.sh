#!/bin/bash
set -e

# Stop and disable the service before removal.
#
# is-active and is-enabled exit non-zero for a service that was never started or enabled, which
# under `set -e` aborted the whole script and left the package half-removed. Tolerate both.
if command -v systemctl > /dev/null; then
    if systemctl is-active --quiet tasch; then
        systemctl stop tasch || true
    fi
    if systemctl is-enabled --quiet tasch 2>/dev/null; then
        systemctl disable tasch || true
    fi
fi
