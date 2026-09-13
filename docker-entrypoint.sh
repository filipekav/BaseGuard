#!/bin/sh
set -eu
umask 077

# Root is used only to prepare dedicated bind mounts created by CasaOS.
# The application is always executed as the unprivileged UID/GID 10001.
startup_uid=$(id -u)
as_app() {
    if [ "$startup_uid" = 0 ]; then gosu 10001:10001 "$@"; else "$@"; fi
}

# Maintenance commands do not require all application volumes.
if [ "$#" -gt 0 ]; then
    if [ "$startup_uid" = 0 ]; then exec gosu 10001:10001 /usr/local/bin/baseguard "$@"; fi
    exec /usr/local/bin/baseguard "$@"
fi

fail_access() {
    echo "BaseGuard: sem acesso a $1 como usuario 10001:10001." >&2
    ls -ldn "$1" >&2 2>/dev/null || true
    echo "Confira volume gravavel (RW), proprietario e permissoes no host. Recrie o container com o Compose CasaOS atualizado (usuario inicial 0:0 e capacidades padrao do Docker)." >&2
    echo "Se o filesystem recusar chown, configure acesso ao UID/GID 10001 na montagem do host. As permissoes nao serao abertas com chmod 777." >&2
    exit 1
}

if ! as_app true; then
    echo "BaseGuard: nao foi possivel assumir UID/GID 10001; confira SETUID/SETGID e recrie usando o Compose atualizado." >&2
    exit 1
fi

adjust_owner() {
    if [ "$startup_uid" = 0 ]; then
        if chown 10001:10001 "$1"; then
            chmod "$2" "$1" || fail_access "$1"
        else
            echo "BaseGuard: chown recusado em $1; verificando acesso efetivo do aplicativo." >&2
        fi
    fi
}

data_dir=${BASEGUARD_DATA_DIR:-/data}
key_file=${BASEGUARD_KEY_FILE:-/secrets/master.key}
key_dir=$(dirname "$key_file")

prepare_directory() {
    if [ "$startup_uid" = 0 ]; then
        case "$1" in /data|/secrets|/backups) ;; *) echo "Bootstrap only supports dedicated /data, /secrets and /backups mounts." >&2; exit 1 ;; esac
    fi
    if [ -L "$1" ]; then echo "Refusing symlink mount directory: $1" >&2; exit 1; fi
    mkdir -p "$1" || fail_access "$1"
    adjust_owner "$1" 0700
    # Creating and removing a real file also detects read-only mounts and ACLs.
    as_app sh -c 'test -r "$1" && test -x "$1" && probe=$(mktemp "$1/.baseguard-access.XXXXXX") && rm "$probe"' sh "$1" || fail_access "$1"
}

prepare_file() {
    if [ -L "$1" ]; then echo "Refusing symlink application file: $1" >&2; exit 1; fi
    [ -e "$1" ] || return 0
    [ -f "$1" ] || fail_access "$1"
    adjust_owner "$1" "$2"
    as_app test -r "$1" || fail_access "$1"
    if [ "$3" = write ]; then as_app test -w "$1" || fail_access "$1"; fi
}

prepare_directory "$data_dir"
prepare_directory "$key_dir"
for file in baseguard.db baseguard.db-wal baseguard.db-shm baseguard.db-journal baseguard.lock; do
    prepare_file "$data_dir/$file" 0600 write
done
prepare_file "$key_file" 0600 read

# Automatic initialization is restricted to the default local destination on
# a fresh installation. Existing instances never recreate a missing marker.
if [ "${BASEGUARD_INIT_LOCAL_DESTINATION:-false}" = "true" ]; then
    prepare_directory /backups
    prepare_file /backups/.baseguard-destination 0644 read
    if [ ! -e "$data_dir/baseguard.db" ] && [ ! -e "$key_file" ] && [ ! -e /backups/.baseguard-destination ]; then
        as_app /usr/local/bin/baseguard init-destination /backups Principal
    fi
fi

if [ "$startup_uid" = 0 ]; then exec gosu 10001:10001 /usr/local/bin/baseguard "$@"; fi
exec /usr/local/bin/baseguard "$@"
