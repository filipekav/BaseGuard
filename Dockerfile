FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=arm64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/baseguard ./cmd/baseguard

FROM ubuntu:24.04
ARG PG_MAJOR=18
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && install -d /usr/share/postgresql-common/pgdg \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
    && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends postgresql-client-${PG_MAJOR} mysql-client-8.0 gosu \
    && apt-get purge -y curl gnupg && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 baseguard && useradd -u 10001 -g baseguard -M -s /usr/sbin/nologin baseguard \
    && mkdir -p /data /secrets /backups && chown -R baseguard:baseguard /data /secrets /backups
ENV BASEGUARD_LISTEN=:8080 BASEGUARD_DATA_DIR=/data BASEGUARD_KEY_FILE=/secrets/master.key
COPY --from=build /out/baseguard /usr/local/bin/baseguard
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
LABEL org.opencontainers.image.source="https://github.com/filipekav/BaseGuard" \
      org.opencontainers.image.title="BaseGuard" \
      org.opencontainers.image.description="Backups locais de PostgreSQL e MySQL para CasaOS"
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["baseguard","healthcheck"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
