#!/bin/sh
# Disposable idle-process comparison; no user volumes or database credentials.
set -eu
name="baseguard-resources-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
measure() {
    image=$1
    docker run -d --name "$name" --network none --read-only \
        --tmpfs /tmp:size=16m,mode=1777 --tmpfs /data --tmpfs /secrets --tmpfs /backups \
        -e BASEGUARD_INIT_LOCAL_DESTINATION=true "$image" >/dev/null
    ready=false
    for n in $(seq 1 30); do
        if docker exec "$name" baseguard healthcheck >/dev/null 2>&1; then ready=true; break; fi
        sleep 1
    done
    if [ "$ready" != true ]; then docker logs "$name"; return 1; fi
    bytes=$(docker image inspect "$image" --format '{{.Size}}')
    memory=$(docker stats --no-stream "$name" --format '{{.MemUsage}}')
    printf '| %s | %s | %s |\n' "$image" "$bytes" "$memory"
    docker rm -f "$name" >/dev/null
}
printf '| Image | Uncompressed bytes | Idle container memory |\n|---|---:|---|\n'
measure baseguard:local
if docker pull ghcr.io/filipekav/baseguard:latest >/dev/null 2>&1; then
    measure ghcr.io/filipekav/baseguard:latest
else
    echo 'Previous public image unavailable; baseline comparison omitted.'
fi
