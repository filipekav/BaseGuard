#!/bin/sh
# Real exFAT regression for the CasaOS automount defaults reported by users.
set -eu
if ! grep -qw exfat /proc/filesystems; then
    echo "exFAT kernel driver is required. On the CI host, run scripts/prepare-exfat-ci.sh first." >&2
    exit 1
fi
name="baseguard-exfat-$$"
tmp=$(mktemp -d)
cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    # Never delete through a still-mounted filesystem if unmount fails.
    if mountpoint -q "$tmp/disk"; then sudo umount "$tmp/disk" || return 1; fi
    sudo rm -rf -- "$tmp"
}
trap cleanup EXIT
mkdir "$tmp/disk" "$tmp/local-data" "$tmp/local-secrets"
truncate -s 128M "$tmp/exfat.img"
mkfs.exfat "$tmp/exfat.img"
sudo mount -t exfat -o loop,uid=300,gid=1000,fmask=0022,dmask=0022 "$tmp/exfat.img" "$tmp/disk"
sudo mkdir "$tmp/disk/data" "$tmp/disk/secrets" "$tmp/disk/backups"

wait_ready() {
    for n in $(seq 1 30); do
        if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then break; fi
        if docker exec "$name" baseguard healthcheck >/dev/null 2>&1; then return 0; fi
        sleep 1
    done
    docker logs "$name"
    return 1
}

for layout in external mixed; do
    if [ "$layout" = external ]; then
        data="$tmp/disk/data"
        secrets="$tmp/disk/secrets"
    else
        data="$tmp/local-data"
        secrets="$tmp/local-secrets"
    fi
    docker run -d --name "$name" --user 0:0 --read-only \
        --tmpfs /tmp:size=16m,mode=1777 --security-opt no-new-privileges:true \
        -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
        -v "$data:/data" -v "$secrets:/secrets" -v "$tmp/disk/backups:/backups" baseguard:local
    wait_ready
    docker exec "$name" sh -c 'test "$(awk "/^Uid:/{print \$2}" /proc/1/status)" = 300'
    docker exec --user 300:1000 "$name" sh -c 'test -r /secrets/master.key && test -r /backups/.baseguard-destination && mkdir /backups/write-test && echo test > /backups/write-test/dump && rm /backups/write-test/dump && rmdir /backups/write-test'
    key_before=$(sudo sha256sum "$secrets/master.key")
    docker restart "$name"
    wait_ready
    test "$key_before" = "$(sudo sha256sum "$secrets/master.key")"
    docker rm -f "$name"
done

# Do not solve root-owned exFAT by running the application as root.
sudo umount "$tmp/disk"
sudo mount -t exfat -o loop,uid=0,gid=0,fmask=0022,dmask=0022 "$tmp/exfat.img" "$tmp/disk"
if docker run --name "$name" --read-only --tmpfs /tmp:size=16m,mode=1777 \
    -e BASEGUARD_INIT_LOCAL_DESTINATION=true \
    -v "$tmp/local-data:/data" -v "$tmp/local-secrets:/secrets" -v "$tmp/disk/backups:/backups" baseguard:local; then
    echo "Expected root-owned exFAT to be refused" >&2; exit 1
fi
docker logs "$name" 2>&1 | grep -F 'pertence a root'
