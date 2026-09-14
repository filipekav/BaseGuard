#!/bin/sh
# CI host only: exfatprogs formats images but does not provide a kernel driver.
set -eu
kernel=$(uname -r)
echo "Preparing exFAT tests for kernel $kernel ($(uname -m))"
sudo apt-get update
sudo apt-get install -y --no-install-recommends exfatprogs kmod

# Built-in or already-installed drivers need no additional package.
if ! grep -qw exfat /proc/filesystems; then
    if ! sudo modprobe exfat; then
        # Match the running kernel, not a metapackage for a newer kernel that
        # would require rebooting the runner before its modules could be used.
        sudo apt-get install -y --no-install-recommends "linux-modules-extra-$kernel"
        sudo depmod -a "$kernel"
        sudo modprobe exfat
    fi
fi
if ! grep -qw exfat /proc/filesystems; then
    echo "exFAT driver unavailable for running kernel $kernel; refusing to skip filesystem tests." >&2
    exit 1
fi
echo "exFAT kernel support ready"
