# syntax=docker/dockerfile:1
# Client image references are locked in clients.lock.json; CI checks parity.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/baseguard ./cmd/baseguard

FROM postgres:12.22-bookworm@sha256:2f2a8c2a7d10862e7fba2602e304523554f9df8244c632dafe2628ccb398fb5c AS postgres-12
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-12 psql pg_dump pg_restore

FROM postgres:13.23-bookworm@sha256:f0cffcc050a9f1f3c78a9968e221badc1cdd02e2ae15b1de9b12bee1ba5ea2db AS postgres-13
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-13 psql pg_dump pg_restore

FROM postgres:14.24-bookworm@sha256:185c7c7a36448bcb7e0d3d6aef97ac973ddfae0cc4fa29581ce4e789988a74b1 AS postgres-14
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-14 psql pg_dump pg_restore

FROM postgres:15.19-bookworm@sha256:5d1d70e254e3c5d7d76847a9deebb18478cd518df37abf6b278d4bdb1fe5d96c AS postgres-15
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-15 psql pg_dump pg_restore

FROM postgres:16.15-bookworm@sha256:bb3e1a57e5407e0a5280b4211980a5e537f4abd234a87014ac979849a78dd825 AS postgres-16
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-16 psql pg_dump pg_restore

FROM postgres:17.11-bookworm@sha256:051f7b7b3abdd564d5d1bd1e8c4b9c1b6e77087d1dd22020ede611c096a272e0 AS postgres-17
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-17 psql pg_dump pg_restore

FROM postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af AS postgres-18
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh postgres-18 psql pg_dump pg_restore

FROM mysql:8.0@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b AS mysql-8_0
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mysql-8.0 mysql mysqldump

FROM mysql:8.4@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a AS mysql-8_4
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mysql-8.4 mysql mysqldump

FROM mariadb:10.6@sha256:553351e202c5f01c9e703d7d1d2b037a0aedb43c4fed79b3615d9183655f02c5 AS mariadb-10_6
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mariadb-10.6 mariadb mariadb-dump

FROM mariadb:10.11@sha256:07c0aaff7396b74cb7975cba78257178d188e30f531a5db2b617c48beef13c41 AS mariadb-10_11
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mariadb-10.11 mariadb mariadb-dump

FROM mariadb:11.4@sha256:80494b9810694179889f7281ec44ca928241df577159c0356a1070e2e94616a1 AS mariadb-11_4
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mariadb-11.4 mariadb mariadb-dump

FROM mariadb:11.8@sha256:2d2f4095530294735a857cfe22bb101e19b0849b416911c796ec4aa81b164a62 AS mariadb-11_8
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mariadb-11.8 mariadb mariadb-dump

FROM mariadb:12.3@sha256:ab1c3dd381940233af12512b97d47b508fd3a0f17fbe3ba388739b7bc17cbc0b AS mariadb-12_3
USER root
COPY scripts/collect-client.sh /tmp/collect-client.sh
RUN sh /tmp/collect-client.sh mariadb-12.3 mariadb mariadb-dump

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 baseguard && useradd -u 10001 -g baseguard -M -s /usr/sbin/nologin baseguard \
    && mkdir -p /data /secrets /backups && chown -R baseguard:baseguard /data /secrets /backups
COPY --from=postgres-12 /out/postgres-12 /opt/baseguard/clients/postgres-12
COPY --from=postgres-13 /out/postgres-13 /opt/baseguard/clients/postgres-13
COPY --from=postgres-14 /out/postgres-14 /opt/baseguard/clients/postgres-14
COPY --from=postgres-15 /out/postgres-15 /opt/baseguard/clients/postgres-15
COPY --from=postgres-16 /out/postgres-16 /opt/baseguard/clients/postgres-16
COPY --from=postgres-17 /out/postgres-17 /opt/baseguard/clients/postgres-17
COPY --from=postgres-18 /out/postgres-18 /opt/baseguard/clients/postgres-18
COPY --from=mysql-8_0 /out/mysql-8.0 /opt/baseguard/clients/mysql-8.0
COPY --from=mysql-8_4 /out/mysql-8.4 /opt/baseguard/clients/mysql-8.4
COPY --from=mariadb-10_6 /out/mariadb-10.6 /opt/baseguard/clients/mariadb-10.6
COPY --from=mariadb-10_11 /out/mariadb-10.11 /opt/baseguard/clients/mariadb-10.11
COPY --from=mariadb-11_4 /out/mariadb-11.4 /opt/baseguard/clients/mariadb-11.4
COPY --from=mariadb-11_8 /out/mariadb-11.8 /opt/baseguard/clients/mariadb-11.8
COPY --from=mariadb-12_3 /out/mariadb-12.3 /opt/baseguard/clients/mariadb-12.3
COPY clients.lock.json /opt/baseguard/clients.lock.json
ENV BASEGUARD_LISTEN=:8080 BASEGUARD_DATA_DIR=/data BASEGUARD_KEY_FILE=/secrets/master.key
ENV BASEGUARD_CLIENTS_DIR=/opt/baseguard/clients
COPY --from=build /out/baseguard /usr/local/bin/baseguard
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
# Fail during the build if a copied executable cannot run on the target platform.
RUN for client in /opt/baseguard/clients/*/bin/*; do "$client" --version || exit 1; done
LABEL org.opencontainers.image.source="https://github.com/filipekav/BaseGuard" \
      org.opencontainers.image.title="BaseGuard" \
      org.opencontainers.image.description="Backups de PostgreSQL, MySQL e MariaDB para CasaOS"
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["baseguard","healthcheck"]
ENTRYPOINT ["/bin/sh", "/usr/local/bin/docker-entrypoint.sh"]
