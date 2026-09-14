#!/bin/sh
set -eu
umask 077

# Root is used only to prepare dedicated bind mounts created by CasaOS.
# The application always runs without root. exFAT ownership is mount-wide.
startup_uid=$(id -u)
app_uid=10001
app_gid=10001
data_dir=${BASEGUARD_DATA_DIR:-/data}
key_file=${BASEGUARD_KEY_FILE:-/secrets/master.key}
key_dir=$(dirname "$key_file")

is_exfat() {
    [ "$(stat -f -c %T "$1" 2>/dev/null)" = exfat ]
}

# Read only the dedicated volumes. Never change the disk's mount options.
if [ "$startup_uid" = 0 ]; then
    selected_owner=
    for directory in "$data_dir" "$key_dir" /backups; do
        case "$directory" in /data|/secrets|/backups) ;; *) continue ;; esac
        [ ! -L "$directory" ] || continue
        if is_exfat "$directory"; then
            owner=$(stat -c %u "$directory")
            group=$(stat -c %g "$directory")
            if [ "$owner" = 0 ]; then
                echo "BaseGuard: exFAT em $directory pertence a root. A montagem precisa de um proprietario sem root para permitir acesso privado ao aplicativo." >&2
                exit 1
            fi
            if [ -n "$selected_owner" ] && [ "$selected_owner" != "$owner:$group" ]; then
                echo "BaseGuard: volumes exFAT com proprietarios diferentes; use montagens com o mesmo UID/GID." >&2
                exit 1
            fi
            selected_owner=$owner:$group
            app_uid=$owner
            app_gid=$group
        fi
    done
    if [ -n "$selected_owner" ]; then
        echo "BaseGuard: exFAT detectado; usuario do aplicativo $app_uid:$app_gid, conforme o proprietario da montagem." >&2
    fi
else
    app_uid=$startup_uid
    app_gid=$(id -g)
fi

as_app() {
    if [ "$startup_uid" = 0 ]; then gosu "$app_uid:$app_gid" "$@"; else "$@"; fi
}

# Maintenance commands do not require all application volumes.
if [ "$#" -gt 0 ]; then
    if [ "$startup_uid" = 0 ]; then exec gosu "$app_uid:$app_gid" /usr/local/bin/baseguard "$@"; fi
    exec /usr/local/bin/baseguard "$@"
fi

fail_access() {
    echo "BaseGuard: sem acesso a $1 como usuario $app_uid:$app_gid." >&2
    ls -ldn "$1" >&2 2>/dev/null || true
    echo "Confira volume gravavel (RW), proprietario e permissoes no host. Recrie o container com o Compose CasaOS atualizado (usuario inicial 0:0 e capacidades padrao do Docker)." >&2
    echo "O volume precisa permitir escrita ao UID/GID $app_uid:$app_gid. As permissoes nao serao abertas com chmod 777." >&2
    exit 1
}

if ! as_app true; then
    echo "BaseGuard: nao foi possivel assumir UID/GID $app_uid:$app_gid; confira SETUID/SETGID e recrie usando o Compose atualizado." >&2
    exit 1
fi

adjust_owner() {
    # chmod/chown cannot set per-file Unix permissions on exFAT.
    if is_exfat "$1"; then return 0; fi
    if [ "$startup_uid" = 0 ]; then
        if chown "$app_uid:$app_gid" "$1"; then
            chmod "$2" "$1" || fail_access "$1"
        else
            echo "BaseGuard: chown recusado em $1; verificando acesso efetivo do aplicativo." >&2
        fi
    fi
}

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

if [ -s "$data_dir/baseguard.db" ] && [ ! -f "$key_file" ]; then
    echo "BaseGuard: banco existente sem a chave $key_file. Se mudou os volumes, restaure a pasta de secrets original junto com a pasta data; uma chave nova nao recupera as credenciais." >&2
    exit 1
fi

# Automatic initialization is restricted to the default local destination on
# a fresh installation. Existing instances never recreate a missing marker.
if [ "${BASEGUARD_INIT_LOCAL_DESTINATION:-false}" = "true" ]; then
    prepare_directory /backups
    prepare_file /backups/.baseguard-destination 0644 read
    if [ ! -e "$data_dir/baseguard.db" ] && [ ! -e "$key_file" ] && [ ! -e /backups/.baseguard-destination ]; then
        as_app /usr/local/bin/baseguard init-destination /backups Principal
    elif [ ! -e /backups/.baseguard-destination ]; then
        echo "BaseGuard: /backups sem marcador em instalacao existente. Se mudou o volume, copie o destino original completo, incluindo .baseguard-destination. O painel pode iniciar, mas este destino nao aceitara backups ate ser configurado." >&2
    fi
fi

if [ "$startup_uid" = 0 ]; then exec gosu "$app_uid:$app_gid" /usr/local/bin/baseguard "$@"; fi
exec /usr/local/bin/baseguard "$@"
