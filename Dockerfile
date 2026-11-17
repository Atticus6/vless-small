# syntax=docker/dockerfile:1

# 构建阶段跑在原生平台, 通过 GOOS/GOARCH 交叉编译目标架构, 多架构构建不走 QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
# 证书在构建平台安装, 运行阶段直接 COPY, 不在目标架构执行任何命令.
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN set -eu; \
    export CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    if [ "$TARGETARCH" = "arm" ]; then \
        case "$TARGETVARIANT" in \
            v6) export GOARM=6 ;; \
            v7|"") export GOARM=7 ;; \
            *) echo "unsupported ARM variant: $TARGETVARIANT" >&2; exit 1 ;; \
        esac; \
    fi; \
    go build -trimpath -ldflags="-s -w" -o /out/vless-small .

# 运行阶段使用 Alpine; 全程无 RUN, 多架构构建不需要 QEMU.
FROM alpine:latest

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

WORKDIR /app
COPY --from=builder /out/vless-small /app/vless-small

# 数字 UID/GID 无需 adduser, 也不依赖 passwd 条目.
USER 10001:10001
EXPOSE 8080
ENV PORT=8080

ENTRYPOINT ["/app/vless-small"]
