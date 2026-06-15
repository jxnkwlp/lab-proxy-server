# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

RUN apk add --no-cache ca-certificates curl unzip

COPY go.mod go.sum ./
RUN go mod download

COPY src ./src

RUN set -eux; \
    curl -fsSL "https://codeload.github.com/MetaCubeX/Yacd-meta/zip/refs/heads/gh-pages" -o /tmp/dashboard.zip; \
    rm -rf /tmp/dashboard src/static/dashboard; \
    mkdir -p /tmp/dashboard src/static/dashboard; \
    unzip -q /tmp/dashboard.zip -d /tmp/dashboard; \
    dashboard_root="$(find /tmp/dashboard -type f -name index.html -exec dirname {} \; | head -n 1)"; \
    if [ -z "$dashboard_root" ]; then \
      echo "Downloaded dashboard archive does not contain index.html" >&2; \
      exit 1; \
    fi; \
    cp -a "$dashboard_root"/. src/static/dashboard/

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/clash-server ./src

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/clash-server /usr/local/bin/clash-server

VOLUME ["/app/data"]

EXPOSE 7890 7891 7892 9090

ENTRYPOINT ["clash-server"]
