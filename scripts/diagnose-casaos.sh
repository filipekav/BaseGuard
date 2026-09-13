#!/bin/sh
# Read-only diagnostics: no environment values, keys, database or dump contents.
set -eu
name=${1:-baseguard-baseguard-1}
docker inspect --format 'Image={{.Config.Image}} ID={{.Image}} User={{.Config.User}} CapDrop={{json .HostConfig.CapDrop}} CapAdd={{json .HostConfig.CapAdd}} Status={{.State.Status}} Error={{.State.Error}}' "$name"
docker inspect --format '{{range .Mounts}}{{printf "%s -> %s RW=%v\n" .Source .Destination .RW}}{{end}}' "$name"
for target in /data /secrets /backups; do
    source=$(docker inspect --format "{{range .Mounts}}{{if eq .Destination \"$target\"}}{{.Source}}{{end}}{{end}}" "$name")
    [ -n "$source" ] || continue
    stat -c '%n owner=%u:%g mode=%a' -- "$source"
    if command -v findmnt >/dev/null 2>&1; then findmnt -T "$source" -o TARGET,FSTYPE || true; fi
done
