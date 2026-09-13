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
    --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE --cap-add SETUID --cap-add SETGID \
    --security-opt no-new-privileges:true \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/data:/data" -v "$tmp/secrets:/secrets" -v "$tmp/backups:/backups" baseguard:local
ready=false
for n in $(seq 1 30); do
    if docker exec "$name" baseguard healthcheck; then ready=true; break; fi
    sleep 1
done
if [ "$ready" != true ]; then docker logs "$name"; exit 1; fi
docker exec "$name" sh -c 'test "$(awk "/^Uid:/{print \$2}" /proc/1/status)" = 10001'
sudo test -f "$tmp/backups/.baseguard-destination"
sudo test -f "$tmp/secrets/master.key"
docker stop "$name"
sudo rm "$tmp/backups/.baseguard-destination"
docker start "$name"
sleep 2
# Restarting an existing app must not reinitialize an absent destination.
if sudo test -e "$tmp/backups/.baseguard-destination"; then exit 1; fi
