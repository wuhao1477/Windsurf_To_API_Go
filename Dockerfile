# syntax=docker/dockerfile:1.7

FROM node:20-bookworm-slim AS web-builder

WORKDIR /src/web

COPY web/package.json web/pnpm-lock.yaml ./

RUN corepack enable && pnpm install --frozen-lockfile

COPY web/ ./

RUN pnpm build


FROM golang:1.26.2-bookworm AS go-builder

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY . .
COPY --from=web-builder /src/internal/web/dist /src/internal/web/dist

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/windsurfapi ./cmd/windsurfapi


FROM debian:bookworm-slim

ARG WINDSURF_LS_URL="https://windsurf-stable.codeiumdata.com/linux-x64/stable/abcd9c8664da5af505557f3b327b5537400635f2/Windsurf-linux-x64-2.0.61.tar.gz"

ENV PORT=3003 \
    BIND_HOST=0.0.0.0 \
    LS_PORT=42100 \
    LS_BINARY_PATH=/app/bin/language_server_linux_x64 \
    LS_DOWNLOAD_URL=${WINDSURF_LS_URL}

WORKDIR /data

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl tar \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system windsurfapi \
    && useradd --system --gid windsurfapi --home /data --shell /usr/sbin/nologin windsurfapi \
    && mkdir -p /app/bin /data /opt/windsurf/data/db /tmp/windsurf-workspace

COPY --from=go-builder /out/windsurfapi /app/windsurfapi
COPY .env.example /app/.env.example
COPY bin/README.md /app/bin/README.md
COPY scripts/docker/fetch-language-server.sh /usr/local/bin/fetch-language-server

RUN chmod +x /app/windsurfapi /usr/local/bin/fetch-language-server \
    && /usr/local/bin/fetch-language-server /app/bin/language_server_linux_x64 \
    && chown -R windsurfapi:windsurfapi /app /data /opt/windsurf /tmp/windsurf-workspace

VOLUME ["/data", "/opt/windsurf/data", "/tmp/windsurf-workspace"]

EXPOSE 3003

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null || exit 1

USER windsurfapi

CMD ["/app/windsurfapi"]
