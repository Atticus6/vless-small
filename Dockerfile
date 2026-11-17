# syntax=docker/dockerfile:1

# 构建阶段跑在原生平台, 通过 GOOS/GOARCH 交叉编译目标架构, 多架构构建不走 QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/vless-small .

# 运行阶段使用 Alpine, 仅保留二进制和 CA 证书, 非 root 用户运行.
FROM alpine:latest

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 app

WORKDIR /app
COPY --from=builder /out/vless-small /app/vless-small

USER app
EXPOSE 8080
ENV PORT=8080

ENTRYPOINT ["/app/vless-small"]
