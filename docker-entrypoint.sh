#!/bin/sh
set -eu

# Root is used only to prepare dedicated bind mounts created by CasaOS.
# The application is always executed as the unprivileged UID/GID 10001.
if [ "$(id -u)" != "0" ]; then
    exec /usr/local/bin/baseguard "$@"
fi

data_dir=${BASEGUARD_DATA_DIR:-/data}
key_file=${BASEGUARD_KEY_FILE:-/secrets/master.key}
key_dir=$(dirname "$key_file")

prepare_directory() {
    case "$1" in /data|/secrets|/backups) ;; *) echo "Bootstrap only supports dedicated /data, /secrets and /backups mounts." >&2; exit 1 ;; esac
    if [ -L "$1" ]; then echo "Refusing symlink mount directory: $1" >&2; exit 1; fi
    mkdir -p "$1"
    chown 10001:10001 "$1"
    chmod 0700 "$1"
}

prepare_directory "$data_dir"
prepare_directory "$key_dir"

# Automatic initialization is restricted to the default local destination on
# a fresh installation. Existing instances never recreate a missing marker.
if [ "${BASEGUARD_INIT_LOCAL_DESTINATION:-false}" = "true" ]; then
    prepare_directory /backups
    if [ ! -e "$data_dir/baseguard.db" ] && [ ! -e "$key_file" ] && [ ! -e /backups/.baseguard-destination ]; then
        gosu 10001:10001 /usr/local/bin/baseguard init-destination /backups Principal
    fi
fi

exec gosu 10001:10001 /usr/local/bin/baseguard "$@"
