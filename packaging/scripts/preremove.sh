#!/bin/bash
set -e

# Stop and disable service before removal
if command -v systemctl > /dev/null; then
    if systemctl is-active tasch > /dev/null; then
        systemctl stop tasch
    fi
    if systemctl is-enabled tasch > /dev/null; then
        systemctl disable tasch
    fi
fi
