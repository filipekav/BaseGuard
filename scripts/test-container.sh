#!/bin/sh
set -eu
name="baseguard-smoke-$$"
tmp=$(mktemp -d)
cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    # The test uses dedicated temporary directories only.
    sudo rm -rf -- "$tmp"
}
trap cleanup EXIT
mkdir "$tmp/data" "$tmp/secrets" "$tmp/backups"
docker run -d --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    --user 0:0 \
    --security-opt no-new-privileges:true \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local
wait_ready() {
    for n in $(seq 1 30); do
        if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then break; fi
        if docker exec "$name" baseguard healthcheck >/dev/null 2>&1; then return 0; fi
        sleep 1
    done
    docker logs "$name"
    return 1
}
wait_ready
docker exec "$name" sh -c 'test "$(awk "/^Uid:/{print \$2}" /proc/1/status)" = 10001'
sudo test -f "$tmp/backups/.baseguard-destination"
sudo test -f "$tmp/secrets/master.key"
docker stop "$name"
# Moving the host paths works when all files (including hidden files) move
# together. Keep the old directories intact, as in a real safe migration.
mkdir "$tmp/moved"
sudo cp -a "$tmp/data" "$tmp/secrets" "$tmp/backups" "$tmp/moved/"
docker rm "$name"
docker run -d --name "$name" --user 0:0 --read-only --tmpfs /tmp:size=16m,mode=1777 \
    --security-opt no-new-privileges:true -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/moved/data:/data" -v "$tmp/moved/secrets:/secrets" -v "$tmp/moved/backups:/backups" baseguard:local
wait_ready
sudo cmp "$tmp/secrets/master.key" "$tmp/moved/secrets/master.key"
sudo cmp "$tmp/backups/.baseguard-destination" "$tmp/moved/backups/.baseguard-destination"
docker rm -f "$name"

# An existing database must never silently get a replacement encryption key.
mkdir "$tmp/empty-secrets"
if docker run --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/moved/data:/data" -v "$tmp/empty-secrets:/secrets" -v "$tmp/moved/backups:/backups" baseguard:local; then
    echo "Expected missing original key to fail" >&2; exit 1
fi
docker logs "$name" 2>&1 | grep -F 'banco existente sem a chave'
if sudo test -e "$tmp/empty-secrets/master.key"; then exit 1; fi
docker rm -f "$name"

docker run -d --name "$name" --user 0:0 --read-only --tmpfs /tmp:size=16m,mode=1777 \
    --security-opt no-new-privileges:true -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local
wait_ready
docker stop "$name"
# Existing installations may contain root-owned files, including a private key.
key_before=$(sudo sha256sum "$tmp/secrets/master.key")
sudo chown -R 0:0 "$tmp/data" "$tmp/secrets" "$tmp/backups"
sudo chmod 0600 "$tmp/data/baseguard.db" "$tmp/secrets/master.key" "$tmp/backups/.baseguard-destination"
docker start "$name"
wait_ready
test "$key_before" = "$(sudo sha256sum "$tmp/secrets/master.key")"
test "$(sudo stat -c '%u:%g:%a' "$tmp/secrets/master.key")" = 10001:10001:600
docker stop "$name"
sudo rm "$tmp/backups/.baseguard-destination"
docker start "$name"
wait_ready
# Restarting an existing app must not reinitialize an absent destination.
if sudo test -e "$tmp/backups/.baseguard-destination"; then exit 1; fi
docker rm -f "$name"

# The manually prepared local Compose profile runs without root or capabilities.
docker run -d --name "$name" --user 10001:10001 --cap-drop ALL \
    --read-only --tmpfs /tmp:size=16m,mode=1777 --security-opt no-new-privileges:true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local
wait_ready
docker rm -f "$name"

# A filesystem may reject chown while access is already correctly configured.
# Dropping CHOWN/FOWNER models this without requiring a NAS in CI.
docker run -d --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    --cap-drop CHOWN --cap-drop FOWNER --security-opt no-new-privileges:true \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local
wait_ready
docker rm -f "$name"

# Root bootstrap without permission to repair a root-owned destination must
# fail explicitly, rather than loop or make the directory world-writable.
sudo chown 0:0 "$tmp/backups"
sudo chmod 0700 "$tmp/backups"
if docker run --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    --cap-drop CHOWN --cap-drop FOWNER --security-opt no-new-privileges:true \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local; then
    echo "Expected inaccessible destination to fail" >&2; exit 1
fi
docker logs "$name" 2>&1 | grep -F 'sem acesso a /backups'
test "$(sudo stat -c %a "$tmp/backups")" = 700
docker rm -f "$name"

sudo chown 10001:10001 "$tmp/backups"
if docker run --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups:ro" baseguard:local; then
    echo "Expected read-only destination to fail" >&2; exit 1
fi
docker logs "$name" 2>&1 | grep -F 'sem acesso a /backups'
