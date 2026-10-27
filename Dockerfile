# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
ARG VERSION=dev
ARG COMMIT=unknown
RUN cd server && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Version=${VERSION} -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Commit=${COMMIT} -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /out/matrix-server ./cmd/matrix-server && \
    (go build -trimpath -ldflags "-s -w" -o /out/matrix-agent ./cmd/matrix-agent 2>/dev/null || true)

# ---- runtime ----
FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates tzdata openssh-client && \
    pip install --no-cache-dir ansible-core==2.21.5 && \
    ansible-playbook --version >/dev/null && \
    useradd --uid 10001 --create-home --home-dir /home/matrix --shell /usr/sbin/nologin matrix && \
    rm -rf /var/lib/apt/lists/*
ENV TZ=Asia/Shanghai MATRIX_STATIC_DIR=/app/public HOME=/home/matrix
WORKDIR /app
COPY --from=build /out/ /app/bin/
COPY public/ /app/public/
COPY server/config.example.yaml /etc/matrix/config.yaml
USER matrix
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=20s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2)" || exit 1
ENTRYPOINT ["/app/bin/matrix-server"]
CMD ["serve", "-config", "/etc/matrix/config.yaml"]
