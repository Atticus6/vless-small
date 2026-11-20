# syntax=docker/dockerfile:1

# 构建阶段跑在原生平台, 通过 GOOS/GOARCH 交叉编译目标架构, 多架构构建不走 QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Release 构建可传 --build-arg VERSION=v0.1.5 写入版本号；本地默认为 dev。
ARG VERSION=dev
# 临时隧道实现：默认构建纯 Go 版本；传 cloudflared 改为内嵌官方 cloudflared。
# 例：docker build --build-arg BUILD_TAGS=cloudflared -t vless-small:cloudflared .
# 容器里始终追加 nobbr：镜像不应修改宿主机内核参数，BBR 代码整段不编进镜像。
ARG BUILD_TAGS=

WORKDIR /src
# 证书在构建平台安装, 运行阶段直接 COPY, 不在目标架构执行任何命令.
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 容器构建固定附加 nobbr 标签：BBR 调优只对裸机/VPS 有意义，镜像里既没有
# 权限也不该改宿主机 sysctl，因此把实现整体剔除。
RUN set -eu; \
    export CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    tags="nobbr"; \
    if [ -n "$BUILD_TAGS" ]; then tags="$BUILD_TAGS,nobbr"; fi; \
    if [ "$TARGETARCH" = "arm" ]; then \
        case "$TARGETVARIANT" in \
            v6) export GOARM=6 ;; \
            v7|"") export GOARM=7 ;; \
            *) echo "unsupported ARM variant: $TARGETVARIANT" >&2; exit 1 ;; \
        esac; \
    fi; \
    go build -tags "$tags" -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/vless-small .

# 运行阶段使用 Alpine; 全程无 RUN, 多架构构建不需要 QEMU.
FROM alpine:latest

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

WORKDIR /app
# /app/bin 用 --chown 交给运行用户：Web 下发的自更新需要在同目录创建临时
# 文件并 rename 覆盖 vless-small。只读 rootfs 的部署方式无法自更新。
COPY --chown=10001:10001 --from=builder /out /app/bin

# 数字 UID/GID 无需 adduser, 也不依赖 passwd 条目.
USER 10001:10001
EXPOSE 8080
ENV PORT=8080

ENTRYPOINT ["/app/bin/vless-small"]
