#!/bin/sh
# Disposable integration servers only. All certificates use ephemeral CI keys.
set -eu
mkdir -p /tmp/baseguard-tls
cp /certs/* /tmp/baseguard-tls/
chmod 0600 /tmp/baseguard-tls/server.key
if [ "$BASEGUARD_TEST_ENGINE" = postgres ]; then
    chown -R postgres:postgres /tmp/baseguard-tls
    if [ "${BASEGUARD_TEST_NO_TLS:-0}" = 1 ]; then exec /usr/local/bin/docker-entrypoint.sh postgres -c ssl=off; fi
    exec /usr/local/bin/docker-entrypoint.sh postgres \
        -c ssl=on -c ssl_cert_file=/tmp/baseguard-tls/server.crt -c ssl_key_file=/tmp/baseguard-tls/server.key
fi
chown -R mysql:mysql /tmp/baseguard-tls
server=mysqld
if [ "$BASEGUARD_TEST_ENGINE" = mariadb ]; then server=mariadbd; fi
if [ "${BASEGUARD_TEST_NO_TLS:-0}" = 1 ]; then
    if [ "$BASEGUARD_TEST_ENGINE" = mysql ]; then
        # MySQL 8.4 removed --skip-ssl; an empty protocol list also works on 5.7/8.0.
        exec /usr/local/bin/docker-entrypoint.sh "$server" --tls-version=
    fi
    exec /usr/local/bin/docker-entrypoint.sh "$server" --skip-ssl
fi
exec /usr/local/bin/docker-entrypoint.sh "$server" \
    --ssl-ca=/tmp/baseguard-tls/ca.crt --ssl-cert=/tmp/baseguard-tls/server.crt --ssl-key=/tmp/baseguard-tls/server.key
