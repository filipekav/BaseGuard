#!/bin/sh
# Run only inside a pinned official database image at build time.
# Copy client executables and their dynamic dependency closure, never the server.
set -eu
profile=$1
shift
root="/out/$profile"
mkdir -p "$root/bin" "$root/libexec" "$root/lib" "$root/plugins" "$root/licenses" "$root/share/charsets"
dependencies() {
    ldd "$1" > /tmp/baseguard-ldd
    if grep -q 'not found' /tmp/baseguard-ldd; then cat /tmp/baseguard-ldd >&2; exit 1; fi
    awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}' /tmp/baseguard-ldd |
    while IFS= read -r library; do cp -L "$library" "$root/lib/"; done
}
for program in "$@"; do
    executable=$(command -v "$program")
    # Debian's /usr/bin/psql can be a Perl pg_wrapper, not the actual ELF.
    case "$profile" in postgres-*) executable="/usr/lib/postgresql/${profile#postgres-}/bin/$program" ;; esac
    executable=$(readlink -f "$executable")
    cp "$executable" "$root/libexec/$program"
    dependencies "$executable"
done
for directory in /usr/share/mysql/charsets /usr/share/mariadb/charsets; do
    [ -d "$directory" ] || continue
    cp -a "$directory/." "$root/share/charsets/"
done

# Optional authentication plugins, including their own dependencies.
for directory in /usr/lib64/mysql/plugin /usr/lib/mysql/plugin /usr/lib/*/libmariadb3/plugin /usr/lib/*/mariadb19/plugin; do
    [ -d "$directory" ] || continue
    for plugin in "$directory"/authentication_*.so "$directory"/client_ed25519.so "$directory"/dialog.so "$directory"/mysql_clear_password.so "$directory"/caching_sha2_password.so "$directory"/sha256_password.so; do
        [ -f "$plugin" ] || continue
        cp -L "$plugin" "$root/plugins/"
        dependencies "$plugin"
    done
done
for library in /lib/*/libnss_dns.so.2 /lib/*/libnss_files.so.2 /lib64/libnss_dns.so.2 /lib64/libnss_files.so.2; do
    [ -f "$library" ] || continue
    cp -L "$library" "$root/lib/"
done
loader=
for file in "$root"/lib/ld-linux*.so*; do
    [ -f "$file" ] || continue
    loader=$(basename "$file")
done
[ -n "$loader" ] || { echo 'Missing ELF interpreter' >&2; exit 1; }
for program in "$@"; do
    # The matching loader avoids mixing glibc versions from different images.
    cat > "$root/bin/$program" <<EOF
#!/bin/sh
set -eu
root=\$(CDPATH= cd -- "\$(dirname -- "\$0")/.." && pwd)
EOF
    case "$profile" in
        postgres-*) printf 'exec "$root/lib/%s" --library-path "$root/lib" "$root/libexec/%s" "$@"\n' "$loader" "$program" >> "$root/bin/$program" ;;
        *) cat >> "$root/bin/$program" <<EOF
case "\${1:-}" in
    --defaults-file=*) defaults=\$1; shift; exec "\$root/lib/$loader" --library-path "\$root/lib" "\$root/libexec/$program" "\$defaults" "--plugin-dir=\$root/plugins" "--character-sets-dir=\$root/share/charsets" "\$@" ;;
esac
exec "\$root/lib/$loader" --library-path "\$root/lib" "\$root/libexec/$program" "--plugin-dir=\$root/plugins" "--character-sets-dir=\$root/share/charsets" "\$@"
EOF
        ;;
    esac
    chmod 0755 "$root/bin/$program"
done
# Preserve distribution-provided license notices for clients and dependencies.
for directory in /usr/share/doc /usr/share/licenses; do
    [ -d "$directory" ] || continue
    cp -a "$directory" "$root/licenses/"
done
