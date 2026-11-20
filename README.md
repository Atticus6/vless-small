# vless-small

最小可用的 **VLESS over WebSocket** 服务端，参考 [go-vless](https://github.com/Atticus6/go-vless) 精简而来。

只保留三件事：WebSocket 握手、VLESS 头解析 + UUID 认证、TCP/UDP 转发。
没有 TLS、隧道、管理后台和流量统计。

## 代码结构

| 路径 | 职责 |
| --- | --- |
| `main.go` | 组装 `app`/`tunnel`/`cert`，启动 HTTP 服务与优雅退出 |
| `config.go` | 命令行/环境变量解析、路径规范化 |
| `internal/app/` | 核心服务：`server.go`（用户表、健康检查）、`websocket.go`、`protocol.go`、`node.go`（CONFIG、上报、设备同步）、`push.go`、`update.go`、`platform.go`、`users.go`、`constants.go`；`egress*.go` 负责出口 IP 检测与 `/ipN` 路径选择，`traffic.go` 负责按设备统计与上报流量 |
| `internal/tunnel/` | 临时隧道控制器：默认使用 `internal/quicktunnel` 纯 Go 实现；`-tags cloudflared` 时内嵌官方 cloudflared supervisor |
| `internal/quicktunnel/` | 纯 Go 临时隧道客户端：申请、QUIC 边缘连接、Cap'n Proto RPC 与 HTTP/WS 转发（默认构建使用） |
| `internal/hy2/` | 最小 Hysteria 2 服务端：HTTP/3 认证、`0x401` TCP 流、QUIC datagram UDP 中继 |
| `internal/cert/` | ACME 自动证书：`cert_common.go`、`cert_impl.go`、`cert_stub.go` |
| `internal/platform/` | 运行平台识别（local / Railway / Northflank 等） |
| `version.txt` | 当前版本号，Release workflow 唯一版本来源 |
| `install.sh` | 一键安装脚本，作为 Release 资产发布 |
| `uninstall.sh` | 独立卸载脚本，作为 Release 资产发布 |
| `.github/workflows/release.yml` | push 触发的多平台构建与 GitHub Release |

## 运行

```bash
# 手动 UUID
UUID=14725836-1234-5678-9abc-def012345678 go run .

# 从主控同步设备（CONFIG = <baseURL>_<name>_<token>）
CONFIG=https://dsh.example.com_hk_<token> go run .

# 等价写法
go run . -uuid 14725836-1234-5678-9abc-def012345678 -listen :8080 -path /ws
```

| 参数 / 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` / `PORT` | `:8080` | 监听地址；`PORT=9000` 时监听 `:9000` |
| `-uuid` / `UUID` | — | 逗号分隔的静态 UUID；设置 `CONFIG` 时可选 |
| `-path` / `WS_PATH` | 空（任意路径） | 只接受指定 WS 路径，如 `/ws` |
| `-config` / `CONFIG` | — | `<baseURL>_<name>_<token>`，用于向主控上报并同步设备 |
| `-hy2-listen` / `HY2_LISTEN` | 空（不启动） | 启用 Hysteria 2 入口；端口固定 **UDP 9500**，值本身只作为开关 |
| `-hy2-password` / `HY2_PASSWORD` | — | 可选的共享密码；接主控时客户端用设备 UUID 认证，无需设置 |
| `-hy2-cert` / `HY2_CERT` | 空（自签） | Hysteria 2 的 TLS 证书；留空时自动生成自签证书 |
| `-hy2-key` / `HY2_KEY` | 空（自签） | Hysteria 2 的 TLS 私钥；留空时自动生成自签证书 |
| `-hy2-domain` / `HY2_DOMAIN` | `bing.com` | 自签证书的 CN/SAN |
| `HY2_SKIP_PROBE` | 空 | 设为 `1` 跳过启动时的端口可达性探测 |
| `-tg-secret` / `TG_SECRET` | 空（不启用） | Telegram Desktop 内置 web-proxy 密钥（32 hex / `dd`+32 hex / UUID）；设置后在 HTTPS 端口提供 bridge 页面与 `/api/v1/*` |
| `-version` | `dev` | 打印版本；Release 构建通过 `-ldflags -X main.version` 注入 tag |

## 一键安装

版本号以 `version.txt` 为准。push 命中 Go 源码、`go.mod`/`go.sum`、
`install.sh`、`uninstall.sh` 或 `version.txt` 时触发 GitHub Actions
（`.github/workflows/release.yml`）：

1. 读取 `version.txt`，把最后一段数字 +1 作为本次版本号
   （`v0.1.0` → `v0.1.1`，`1` → `2`）；
2. 运行 `go vet` / `go test`，交叉编译 Linux amd64/arm64 二进制，
   生成 `checksums.txt` 并创建 GitHub Release；
3. 构建和发布成功后，把新版本号写回 `version.txt` 并推送到当前分支
   （提交信息带 `[skip ci]`，不会再次触发构建）。

不需要手动打 tag 或改版本号。如果要调整版本序列，直接修改 `version.txt`
并推送，workflow 会从该值继续 +1。

仓库是公开的。Web 控制台的“VLESS 管理”页仍会生成一条通过主控代理下载的
安装命令，节点不需要 GitHub 凭据（Worker 用 `GITHUB_TOKEN` 访问 Release，
也能避开部分网络下 GitHub 直链不可用的问题）：

```bash
NODE_NAME='hk-01'; NODE_TOKEN='<node token>'; \
  curl -fsSL -H "Authorization: Bearer $NODE_TOKEN" -H "X-Node-Name: $NODE_NAME" \
    "https://dsh.example.com/api/releases/<version>/install.sh" \
  | sudo bash -s -- --config "https://dsh.example.com_hk-01_<node token>" \
      --version <version> \
      --download-base "https://dsh.example.com/api/releases/<version>" --path /ws
```

直接点页面上的“复制安装命令”即可，版本会自动取当前最新 Release。
如果直连 GitHub，仍可使用 `--github-token` / `GITHUB_TOKEN`；部分网络下
GitHub 直链可能不可达，优先使用上面的主控代理。

脚本按 `uname` 选择对应资产并校验 sha256，创建 `vless-small` 系统用户，把配置
写入 `/etc/vless-small/vless-small.env`（0600），注册系统服务，最后用
`http://127.0.0.1:<PORT>/healthz` 做启动检查。

服务管理器自动检测：有 systemd 时写 `/etc/systemd/system/vless-small.service`；
Alpine 等只有 OpenRC 的环境写 `/etc/init.d/vless-small` + `/etc/conf.d/vless-small`
（`supervise-daemon` 负责崩溃重启）。两者都没有时只安装二进制。脚本本身兼容
busybox ash，Alpine 上无需额外安装 bash。

| 参数 | 说明 |
| --- | --- |
| `--config` / `--uuid` | 主控 CONFIG 或静态 UUID，至少提供一个 |
| `--port` | 监听端口，默认 8080 |
| `--path` | WebSocket 路径，如 `/ws`；不填则接受任意路径 |
| `--public-host` / `--public-hosts` | 上报的公网域名，可重复或逗号分隔 |
| `--node-runtime` | 节点上报的 runtime，默认自动识别 |
| `--version` | 指定 Release 版本，默认 latest；使用 `--download-base` 时必填 |
| `--download-base` | Release 资产代理地址（Web 控制台生成）；设置后不需要 GitHub Token |
| `--github-token` | 直连私有仓库时的 GitHub Token；Web 代理模式不需要 |
| `--install-dir` | 二进制目录；root 默认 `/opt/vless-small`（服务用户可写，支持自更新），非 root 默认 `~/.local/bin`；root 安装会在 `/usr/local/bin` 建符号链接。注册服务时目录必须在 `/opt`、`/srv`、`/var/lib` 或 `/usr/local/lib` 下 |
| `--service` / `--no-service` | 强制或禁止注册系统服务（systemd / OpenRC） |
| `--uninstall` | 停止服务并删除配置与二进制 |

```bash
# 下面的示例省略鉴权；Web 代理请像上面一样带节点 Authorization /
# X-Node-Name，直连私有仓库请用 GitHub Token。

# 升级：不带配置参数时保留已有 env 文件
curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
  | sudo bash

# 非 root（没有 systemd 权限）：只安装二进制到 ~/.local/bin
curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
  | bash -s -- --uuid "14725836-1234-5678-9abc-def012345678" --no-service

# 卸载
curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
  | sudo bash -s -- --uninstall
```

服务管理（systemd）：

```bash
systemctl status vless-small
journalctl -u vless-small -f
vless-small -version   # 查看已安装版本
```

服务管理（Alpine / OpenRC）：

```bash
rc-service vless-small status
rc-service vless-small restart
tail -f /var/log/vless-small.log
vless-small -version
```

Alpine 最小系统需要先补依赖（`tar`、`sha256sum` 已由 busybox 提供）：

```bash
apk add --no-cache curl ca-certificates
```

脚本兼容 busybox ash，Alpine 上不需要安装 bash，直接用 `sh` 执行即可：

```bash
# 安装（以 root 执行；--config 换成控制台生成的命令）
curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
  | sudo sh -s -- --config "https://dsh.example.com_hk_<token>" --path /ws

# 卸载
curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/uninstall.sh \
  | sudo sh
```

## 卸载

Release 同时发布 `uninstall.sh`。系统级安装用 root 执行：

```bash
sudo bash uninstall.sh

# 或通过主控代理下载（使用节点的 CONFIG token）
curl -fsSL -H "Authorization: Bearer <node token>" \
  -H "X-Node-Name: <name>" \
  "https://dsh.example.com/api/releases/<version>/uninstall.sh" | sudo bash
```

脚本会停止/禁用并删除系统服务（systemd unit 或 OpenRC init 脚本 + conf.d）、
`/etc/vless-small`、`/opt/vless-small` 和 `/usr/local/bin/vless-small` 符号链接；`--docker` 额外删除名为
`vless-small` 的容器，`--keep-user` 保留服务用户，`--install-dir` 用于
非默认目录。`install.sh --uninstall` 仍然可用。

> 本服务不内置 TLS。生产环境请放在 Nginx / Caddy / Cloudflare 之后，并配置
> WebSocket Upgrade 与较长的读超时；订阅默认使用 443 + `/ws`。

## 测试

```bash
go test ./...

# 可选：真实临时隧道集成测试（申请 *.trycloudflare.com 并验证回源）
TUNNEL_QUICK_INTEGRATION=1 \
go test ./internal/tunnel -run TestQuickTunnelProvisioning -v -timeout 180s
```

临时隧道测试需要出站 UDP 7844（QUIC）或 TCP 7844（HTTP/2）；UDP 受限环境可加 `TUNNEL_PROTOCOL=http2`。

## 自动更新（Web 下发）

节点每次上报都会带上 `version` / `os` / `arch`。主控“VLESS 管理”页点击
“下发更新”后，Worker 把最新 Release 的目标版本和原始二进制 sha256 写入节点
记录；节点在下一次上报（≤60 秒）收到更新指令后：

1. 用自身 `name` + `token` 从主控 `/api/nodes/update` 下载原始二进制
   （Worker 用 `GITHUB_TOKEN` 访问私有 Release，节点不接触 GitHub 凭据）；
2. 校验 sha256，在二进制同目录写临时文件并原子替换；
3. 退出，由 systemd / OpenRC / Docker 按重启策略拉起新版本。

**systemd**：新版安装脚本把二进制放到 `/opt/vless-small`（属主为服务用户），
`/usr/local/bin/vless-small` 是指向它的符号链接，systemd unit 通过
`ReadWritePaths=/opt/vless-small` 允许替换。已有旧节点需要**重跑一次**新版
`install.sh` 完成迁移，否则二进制仍位于 `/usr/local/bin`，服务用户没有写权限。

**OpenRC（Alpine）**：同样安装到 `/opt/vless-small` 并交给服务用户，
`/etc/conf.d/vless-small` 里用 `supervisor="supervise-daemon"` 托管进程，
崩溃后按 `respawn_delay` 重启；自更新替换二进制后服务会被 supervisor 重新拉起。

**Docker**：

```bash
docker build -t vless-small .
docker run -d --restart=always --name vless-small \
  -e CONFIG="https://dsh.example.com_hk_<token>" \
  -p 8080:8080 vless-small
```

镜像内二进制位于 `/app/bin`（属主 10001），容器内可自更新；`docker restart`
后仍使用更新后的二进制。注意：

- `--restart=always` 必须设置，否则替换后进程退出不会自动拉起；
- `docker compose up -d` 重建容器会回到镜像里的版本，需要再次下发更新或重建镜像；
- 只读 rootfs（`--read-only`）无法自更新。

首轮升级：旧版本节点不认识 `update` 字段，需要手动重跑一次安装脚本或更新
镜像，之后 Web 按钮才会生效。Railway 等自动休眠运行时不会后台
轮询，Web 会拒绝下发。

## 出口 IP 选择（runtime=local）

在 Debian / Ubuntu / Alpine 等 Linux 上，如果默认路由网卡配置了多个 IPv4，
可以让不同 WebSocket 路径从不同源 IP 出站：

- `/ip1`：默认出口（与普通 `/ws` 相同）
- `/ip2`、`/ip3`…：默认网卡上的其余 IPv4
- 普通 `/ws` 或 `-path` 配置的路径继续使用默认出口

检测方式不依赖 `ip`、`ifconfig` 或发行版网络配置：

1. 通过 `net.Dial("udp4", "1.1.1.1:80")` 让内核按路由表选择默认源地址；
2. 用 Go 标准库枚举该网卡上的 IPv4，默认地址排第一；
3. UDP 探测失败时解析 `/proc/net/route` 找默认路由网卡（Alpine 容器同样适用）。

管理页节点操作的“启用出口 IP”开关默认关闭。开启后：

- 节点在下一次 60 秒上报时收到 `egress.enabled=true`；
- 节点确认后，订阅才会为每个出口生成 `/ip1`、`/ip2`… 节点；
- 关闭开关后订阅立即停止生成 `/ipN`，节点在下次上报时停止接受这些路径。

示例（5 个地址）：

```text
/ip1  10.0.0.61   默认出口
/ip2  10.0.0.139
/ip3  10.0.0.152
/ip4  10.0.0.185
/ip5  10.0.0.208
```

`GET /<token>/config` 的 `egress` 字段会返回当前检测结果和开关状态。
出口列表在进程启动时检测；网卡地址变化后需要重启 vless-small。
指定出口的连接只走 IPv4；目标为 IPv6 字面量时会连接失败，普通 `/ws` 不受影响。

## 用户流量统计

节点按已认证的 VLESS UUID 统计转发的 payload 字节：

- TCP：WebSocket → 目标为上传，目标 → WebSocket 为下载；
- UDP：按 datagram payload 统计，不含 2 字节长度前缀；
- 多条并发连接按 UUID 原子累加。

每 5 分钟检查一次累计流量，达到阈值才向主控
`POST /api/nodes/traffic` 上报：

```json
{
  "name": "hk",
  "traffic": [
    { "id": "14725836-1234-5678-9abc-def012345678", "up": 1024, "down": 2048 }
  ]
}
```

- 本轮总流量低于 **4 KiB** 时暂缓，计数保留到下一轮；最多跳过 5 次
  （30 分钟）后强制上报，避免微小流量永久不落库；
- 每批最多 200 个设备，避免 Worker 聚合后的用户数据点超过 Analytics Engine
  单次 250 点限制；
- 上报成功才清空累计值；某一批失败时回滚该批及后续批次，下一轮重试；
- 进程收到 SIGTERM 时会尽量 `FlushTraffic` 一次；自更新直接退出，最多丢失
  当前未上报窗口；
- Railway 等自动休眠运行时没有常驻进程，不启动流量上报循环。

主控不查询设备归属，直接把每条记录写入 Analytics Engine：`blob1` 是设备
UUID，`blob2` 是节点 id，`double1/2` 是上传/下载字节，时间由 Analytics
Engine 自动记录。设备或节点已不存在时也照常写入。

## 临时隧道（Cloudflare Quick Tunnel）

默认构建使用 `internal/quicktunnel` 的**纯 Go 实现**（只依赖 quic-go 和
标准库）：进程内完成临时隧道申请、QUIC 边缘连接和 HTTP/WebSocket 转发，
不链接 cloudflared。加 `-tags cloudflared` 时改为内嵌**官方 cloudflared**，
调用其 `supervisor`、`connection`、`orchestration` 等包；该版本对应
**2026.11.1**，连接器上报的 `tunnelBuildVersion` 与之一致，升级依赖时需
同步修改这个常量。两种实现都不需要在节点上安装 cloudflared 二进制。

### 选择隧道实现（编译期）

```bash
go build -o vless-small .                       # 默认：纯 Go Quick Tunnel
go build -tags cloudflared -o vless-small .     # 内嵌官方 cloudflared

docker build -t vless-small .                                              # 默认
docker build --build-arg BUILD_TAGS=cloudflared -t vless-small:cloudflared .
```

两种实现的对外行为一致：主控开关、`/<token>/config` 的 `tunnel` 字段、
`*.trycloudflare.com` 地址和断线重连逻辑都相同。差异：

| 项 | native（默认） | cloudflared（`-tags cloudflared`） |
| --- | --- | --- |
| 依赖 | quic-go + 标准库 | 内嵌官方 cloudflared 及其全部依赖 |
| 二进制体积 | 明显更小（不链接 cloudflared） | 较大 |
| 边缘传输 | 仅 QUIC；UDP 7844 被封时不可用 | QUIC 或 HTTP/2，可自动回落 |
| 边缘位置 | 暂不解析，保持为空 | 上报 `edgeLocation` |
| 协议 / HA / 流控调优 | 不适用 | `TUNNEL_PROTOCOL`、`TUNNEL_HA_CONNECTIONS`、`TUNNEL_QUIC_*` |
| 数据流类型 | 仅 HTTP/WebSocket（VLESS over WS 场景足够） | cloudflared 支持的类型 |

`go.mod` 仍会保留 cloudflared 依赖——`go mod tidy` 会考虑所有构建标签——
但默认二进制不会链接它。Release 默认发布纯 Go 版本；需要 cloudflared
版本时用源码或 Docker 加 `-tags cloudflared` 构建。纯 Go 版本的线上链路
建议先在 DNS 正常的节点上跑一次 `LIVE_TUNNEL=1` 集成测试。

运行时行为：

- 主控的“VLESS 管理 → 临时隧道”按节点开关，默认开启；节点在每次上报
  （≤60 秒）时读取 `tunnel: { enabled }`，启动/停止临时隧道。
- 启用后节点向 Cloudflare 申请 `https://<random>.trycloudflare.com`，
  **地址随进程重启或重新申请变化**，不依赖 Cloudflare 账号里的命名隧道。
- 节点地址随 `/api/nodes/report` 上报，主控写入 `vless.tunnel_address`，
  列表和指标弹框可直接看到当前临时地址。
- 进程启动后的首份上报带 `tunnelReset: true`，主控先清空数据库里的
  旧地址和边缘位置——上一个进程申请的地址在重启后一定失效，不能继续
  出现在订阅里。本次上报若已申请到新地址，会覆盖清空结果。
- 隧道回源到 `http://127.0.0.1:<PORT>`（显式绑定其他地址时回源到该地址），
  接受任意 Host。
- 默认（纯 Go）实现只使用 QUIC，需要出站 UDP 7844；cloudflared 构建在
  UDP 不可用时还能回落 HTTP/2（出站 TCP 7844）。
- cloudflared 构建的协议默认 `auto`，可用
  `TUNNEL_PROTOCOL=quic|http2|auto` 固定。
- cloudflared 构建只在隧道开启时启动 supervisor，关闭后释放其连接和缓冲区。

### 隧道连接与流控

以下参数只对 `-tags cloudflared` 构建生效，默认（纯 Go）构建不读取它们。
临时隧道的默认值按延迟和吞吐优先选取，内存受限的节点可以再收回：

- `HAConnections = 2`：cloudflared 把新流分摊到两条边缘连接上，并发下载
  更容易吃满链路，单条连接抖动时另一条仍可用。内存紧张可用
  `TUNNEL_HA_CONNECTIONS=1` 降回单连接（上限 4）。
- QUIC 连接级/流级流控窗口 **32 MiB / 8 MiB**：高 BDP 链路上不会因为窗口
  太小把吞吐卡在 `窗口/RTT`；需要省内存时用
  `TUNNEL_QUIC_CONN_FLOW_MIB` / `TUNNEL_QUIC_STREAM_FLOW_MIB` 调小（1–256 MiB）。
- 内部日志默认 `error`，避免高频日志分配；排查时用
  `TUNNEL_LOG_LEVEL=info|debug`。
- UDP 7844 被限制、或希望进一步压低内存时可设 `TUNNEL_PROTOCOL=http2`；
  代价是失去 QUIC 的连接迁移和部分弱网优势。
- 不需要隧道时在主控关闭开关，supervisor 会退出并释放连接。

还可以用 Go 运行时限制堆上限（Go 1.19+）。二进制在 `GOGC` 未显式设置时
默认使用 **200**（用更多堆内存换更少的 GC 停顿，偏向延迟和吞吐）；下面是
内存敏感环境里把它收回的写法：

```bash
# systemd unit
Environment=GOMEMLIMIT=96MiB
Environment=GOGC=50

# OpenRC：写进 /etc/vless-small/vless-small.env
GOMEMLIMIT=96MiB
GOGC=50

# Docker
-e GOMEMLIMIT=96MiB -e GOGC=50
```

`GOMEMLIMIT` 是软上限，超过后会更积极地 GC；`GOGC=50` 降低堆增长，
两者都会用一些 CPU 换内存。不要设置得低于实际峰值，否则会频繁 GC。

> 临时隧道不依赖 Cloudflare 账号或 DNS；需要固定入口时请使用管理员配置的
> CDN 域名。

## 自动 HTTPS 证书

主控“VLESS 管理 → 自动证书”填写域名后，节点在下一次上报（≤60 秒）收到
`cert.domain`，并检查本机 80/443 是否空闲：

- 两个端口都空闲：使用 Let's Encrypt（ACME）申请证书，节点直接监听
  `:80`（HTTP-01 校验/跳转）和 `:443`（TLS + WS）。
- 任一端口被占用（例如已有 Nginx/Caddy）：不启动证书，`/<token>/config`
  返回错误，节点上的 `:8080` 服务不受影响。
- 证书缓存在二进制同目录的 `.certs/`，重启不会重新申请。
- 申请成功/失败/端口占用会立即 `POST /api/nodes/cert` 回调主控（name +
  Bearer token），状态、到期时间、签发者和错误写入 `vless` 表；周期上报也会
  携带同一份 `cert` 状态作为兜底。
- 证书有效后主控会在订阅里额外生成一个 `昵称 · 证书` 节点，使用 443 TLS。

限制：

- 仅 **`runtime=local` 的纯 Linux 非 Docker** 环境；Northflank、
  Railway、Render、Fly、Koyeb、Cloudflare、Zeabur 等托管容器/PaaS 运行时
  都不支持，会在上报中返回 `cert.supported=false`。
- 域名必须解析到该节点，且公网能访问 80/443。
- 安装脚本生成的 systemd 服务已加入 `CAP_NET_BIND_SERVICE`，OpenRC 配置用
  `capabilities="cap_net_bind_service"` 达到同样效果，服务用户可绑定低端口；
  手动运行需要 root 或同等 capability。
- 证书状态写入主控后，订阅会额外生成一个 `昵称 · 证书` 节点；域名已经在
  `hosts` 或隧道中时不会重复。

## HTTP 健康检查

- `GET /` 返回 `vless-small ok`
- `GET /healthz` 只返回探活必需信息：

```json
{
  "ok": true,
  "version": "v0.1.3",
  "uptime": "1m23s",
  "uptimeSeconds": 83
}
```

- `GET /<token>/config` 返回完整运行状态（版本、CPU、内存、磁盘、连接数等）。
  `<token>` 就是 CONFIG 的最后一段，和节点上报使用同一个密钥：

```bash
curl -fsS "http://127.0.0.1:8080/${TOKEN}/config"
```

```json
{
  "ok": true,
  "version": "v0.1.3",
  "uptime": "1m23s",
  "uptimeSeconds": 83,
  "goVersion": "go1.27.4",
  "os": "linux",
  "arch": "amd64",
  "hostname": "hk-01",
  "pid": 1234,
  "devices": 5,
  "activeConnections": 2,
  "activeUUIDs": [
    { "uuid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "connections": 2 }
  ],
  "goroutines": 18,
  "cpu": {
    "percent": 1.25,
    "cores": 4,
    "processSeconds": 1.04
  },
  "memory": {
    "allocBytes": 12582912,
    "heapInuseBytes": 10485760,
    "sysBytes": 41943040,
    "rssBytes": 52428800,
    "numGC": 12
  },
  "disk": {
    "path": "/opt/vless-small",
    "totalBytes": 107374182400,
    "usedBytes": 53687091200,
    "freeBytes": 51539607552,
    "usedPercent": 50
  },
  "system": {
    "uptimeSeconds": 98765,
    "cpu": {
      "percent": 12.5,
      "cores": 4,
      "load1": 0.52,
      "load5": 0.58,
      "load15": 0.59
    },
    "memory": {
      "totalBytes": 16777216000,
      "usedBytes": 6291456000,
      "freeBytes": 2097152000,
      "availableBytes": 10485760000,
      "usedPercent": 37.5,
      "swapTotalBytes": 2097152000,
      "swapUsedBytes": 1048576000
    },
    "disk": [
      {
        "path": "/",
        "device": "/dev/sda1",
        "fsType": "ext4",
        "totalBytes": 107374182400,
        "usedBytes": 53687091200,
        "freeBytes": 51539607552,
        "usedPercent": 50
      }
    ]
  },
  "tunnel": {
    "supported": true,
    "url": "https://hk-tunnel.example.com"
  },
  "cert": {
    "supported": true,
    "enabled": true,
    "domain": "node.example.com",
    "expiresAt": "2027-02-01T00:00:00Z",
    "issuer": "R11"
  }
}
```

- 顶层 `cpu` / `memory` / `disk` 是**当前进程**的占用；`system` 是**操作系统**级占用。
- `system.cpu.percent` 是所有核心的平均值：第一次请求返回开机以来的平均值，
  之后按两次请求的 `/proc/stat` 差值计算；`load1/5/15` 来自 `/proc/loadavg`。
- `system.memory` 来自 `/proc/meminfo`，`usedBytes = totalBytes - availableBytes`
  （与 `free` 的 used 列一致）。
- `system.disk` 来自 `/proc/mounts`，只保留 ext2/3/4、xfs、btrfs、zfs、f2fs、
  vfat、ntfs、overlay 等真实文件系统，按设备去重，最多 20 个挂载点。
- 容器中的 `/proc` 通常反映**宿主机**；如果平台注入 lxcfs，则是容器配额。
- token 错误或未配置 CONFIG 时返回 404。token 位于 URL 路径中，反向代理的
  访问日志可能记录它；如果日志会外泄，建议只在受信网络内调用。
- 反向代理需要允许该路径的 GET 请求；如果只放行了 WS 路径，主控的指标弹框
  会读取失败，需要为 `/<token>/config` 增加一条转发规则。
- `devices` 是当前进程接受的 UUID 数量（静态 UUID + 启用设备）；
  `activeConnections` 是已认证的 WebSocket 连接数，`activeUUIDs` 按 UUID
  汇总各自并发连接数（未认证/其他 HTTP 请求不计入）。
- `cpu.percent` 相对单核计算，可能超过 100。第一次请求返回进程启动以来的
  平均值，之后按两次请求之间的 CPU 时间差计算；`processSeconds` 是累计 CPU 秒数。
- `memory.rssBytes` 来自 `/proc/self/statm`，顶层 `disk` 统计二进制所在文件系统。
  CPU、RSS、磁盘和 `system` 信息目前仅在 Linux 构建中采集，其他平台省略对应字段
  （`rssBytes` / `disk` / `system` 为 `omitempty`，CPU 数值为 0）。
- WebSocket 路径（`WS_PATH`）仍要求 `Upgrade: websocket`，普通 HTTP 请求返回 426

## 主控同步

设置 `CONFIG` 后，节点启动时和每 60 秒会：

1. 向 `<baseURL>/api/nodes/report` 上报节点信息；
2. 使用 `Bearer <token>` 认证（`name` + `token` 必须与主控 `vless` 表中的记录一致）；
3. 接收主控返回的所有设备，把启用设备的 `id` 当作 VLESS UUID 接受连接；
4. 读取 `tunnel.enabled` 并启动/停止内置临时隧道客户端。

节点信息由 vless-small 自动收集：

| 字段 | 来源 |
| --- | --- |
| `version` | 构建时通过 `-ldflags -X main.version` 注入的 Release 版本；本地构建为 `dev` |
| `os` / `arch` | `runtime.GOOS` 与 Release 资产架构（仅发布 amd64/arm64） |
| `cert` | 自动证书状态；主控据此写入 `cert_*` 并生成证书节点 |
| `runtime` | `NODE_RUNTIME` / `RUNTIME`，否则根据 `RAILWAY_*`、`RENDER_*`、`FLY_APP_NAME`、`KOYEB_*`、`CF_PAGES*`、`ZEABUR*`、`NF_*` 自动判断 |
| `hosts` | `PUBLIC_HOSTS`（逗号分隔）/ `PUBLIC_HOST`，否则读取平台分配的公网域名；Northflank 使用 `NF_HOSTS` |
| `ipv4` / `ipv6` | 本机公网 IP 查询；可用 `NODE_IPV4` / `NODE_IPV6` 覆盖 |
| `countryCode` / `latitude` / `longitude` | 本机 IP 查询结果 |

只采集稳定的公网地址：Cloudflare Pages 不使用 `CF_PAGES_URL`（单次部署地址），
请通过 `PUBLIC_HOST` 手动指定生产域名。

IP 查询默认请求 `https://ipinfo.io/json`，可用 `IP_LOOKUP_URL` 指向其他服务。
`name` 和 `token` 中不要包含下划线（`CONFIG` 使用下划线分隔）。

在 **Railway** 上不会启动 60 秒后台同步，只在冷启动时同步一次，
避免空闲时持续请求让实例无法自动休眠。其他运行环境（包括 Northflank）仍按 60 秒刷新设备列表。

Northflank 会注入 `NF_HOSTS`（可能包含多个逗号分隔的地址），直接读取即可；
也可以用通用的 `PUBLIC_HOST` / `PUBLIC_HOSTS` 覆盖。

> 节点端始终拉取**全量启用设备 UUID**。Web 的用户/设备节点授权只影响订阅里
> 包含哪些节点，不会缩小节点端的 UUID 列表，因此授权变化不需要通知每个节点。

新建设备时主控会尝试 `POST <WS_PATH>`（Bearer CONFIG token）把完整设备列表
推给已上报公网地址的节点，让新 UUID 立即生效。节点仍按 60 秒周期拉取全量列表。
反向代理需要允许该 WS 路径上的 POST，推送失败不影响设备创建。

## 客户端

明文 WebSocket（或由 Nginx / Cloudflare 终止 TLS 后转发到本服务）：

```
vless://<DEVICE_ID>@<HOST>:<PORT>?encryption=none&security=none&type=ws&path=%2Fws#vless-small
```

`DEVICE_ID` 是主控中设备记录的 `id`（UUID）。走 TLS/WSS 时把 `security` 改成 `tls`，端口换成 443。

## 出站解析与连接

客户端以域名形式下发目标地址时，解析和建连都由节点完成。为了压低
v2rayNG 的 URL 延迟测试和网页首次连接的时间，转发层做了三件事：

- **解析缓存**：同一域名 5 分钟内复用解析结果，失败结果缓存 10 秒；
  过期后先在后台刷新，刷新期间继续返回旧地址，避免每条新连接都等待
  DNS 往返。缓存上限 2048 条，超限时清理过期项。
- **IPv4 优先**：解析结果先试 IPv4，再回落 IPv6（与 sing-box 的
  `prefer_ipv4` 默认值一致）；绑定 `/ipN` 源地址时只保留 IPv4。
- **多地址并行拨号**：CDN 域名通常返回多个地址；首选地址 120ms 内没有
  建连就并行尝试下一个，某个地址立即失败时直接补位，不会因为一个失效
  地址把整条连接卡到超时。

## 启动时启用 BBR（Linux）

Linux 纯本机环境（`runtime=local`，且未检测到 Docker/Podman/Kubernetes
等容器）启动时会尝试把 TCP 拥塞控制切换为 **BBR**：

- 已经是 `bbr` 时直接返回，不重复写内核参数；
- 内核没有加载 `tcp_bbr` 时尝试 `modprobe tcp_bbr`；
- 内核不支持、缺少 `CAP_NET_ADMIN` 或 `modprobe` 不可用时只记录日志，
  不影响服务启动；
- 容器和 PaaS（Railway、Render、Northflank 等）直接跳过——容器内的
  `/proc/sys` 属于宿主机或独立 netns，不应由应用修改。

三层过滤，任何一层不满足都不会动内核：

| 层 | 条件 | 不满足时 |
| --- | --- | --- |
| 编译期 | 没有 `nobbr` 构建标签 | 实现整段不编进二进制 |
| 运行时 | `runtime=local` | 记录 skip 日志 |
| 运行时 | 未检测到容器标记 | 记录 skip 日志 |

`Dockerfile` 固定追加 `nobbr` 标签，镜像里完全不含 BBR 代码：

```bash
docker build -t vless-small .                          # 自动 -tags nobbr
go build -tags nobbr -o vless-small .                  # 手工构建时剔除
go build -o vless-small .                              # 裸机/VPS：包含 BBR
```

安装脚本生成的服务已授予 `CAP_NET_ADMIN`（systemd 的
`AmbientCapabilities` / OpenRC 的 ambient capability）。**已有节点需要
重跑一次 `install.sh` 才会带上这个能力**；之后每次启动都会自动尝试。
手工用 root 运行二进制时不需要额外配置。

## Telegram Desktop 内置 Web Proxy（可选）

节点可以在同一个 HTTPS 端口上提供 Telegram Desktop 的 **内置 web-proxy**
协议（tdesktop web-proxy，与 Cloudflare Workers 版 `eooce/Cloudflare-proxy`
兼容）。开启后 Telegram Desktop 能通过它连到 Telegram WebK，无需传统的
`tg://proxy` MTProto 入口。

### 启用

```bash
# TG_SECRET 可以是 32 位 hex、dd/ee/ef 前缀 + 32 hex，或 UUID（会被转成 hex）。
# 设置后即在 HTTPS 端口挂载 /api/v1/session、/api/v1/ws 与 bridge 页面。
vless-small -uuid <UUID> -tg-secret '<32-hex-secret>' \
  -tg-bridge-host node.example.com

# 或用环境变量：
# TG_SECRET=<secret> TG_BRIDGE_HOST=node.example.com
```

启动时日志会打印可直接复制的 bridge URL：

```
telegram bridge URL: https://node.example.com/?bridge=<43 字符签名>
```

### 在 Telegram Desktop 里使用

1. 设置 → 高级 → 代理设置 → 代理类型选 **内置代理 / MTProto** 的 web 代理模式
   （Telegram Desktop 的 "Use built-in proxy / web proxy"）。
2. 把上面打印的 `https://...?bridge=...` URL 填入代理地址。
3. bridge URL 的签名绑定了域名，域名换了对不上，需要用新域名重新生成。

### 实现说明（`internal/tgproxy/`）

协议分三层：

- **信令**：`POST /api/v1/session` 用 9 字节握手 + HMAC bootstrap token 换取
  session token；`DELETE` 销毁会话；`GET /api/v1/ws` 升级为一条 WebSocket
  "lane"（一条 lane = 一条到 TG DC 的流），子协议为
  `tproxy-lane-v1.<session-token>.<lane-id>`。
- **lane 帧**：`[type:1][sid:3][len:4][payload]`，type 1=open 2=data 3=close
  4=window 流控 17=welcome。
- **TG 数据面**：客户端首个 64 字节是 AES-CTR 加密的伪装头，解密得到
  mode(dd/ee/ef) 与 DC 编号；服务端重新生成伪装头，连到
  `wss://kws{N}[-1].web.telegram.org/apiws`，双向做 AES-CTR 重加密与 TG
  长度分帧。密钥派生（HMAC-SHA256）、token 格式（`exp|nonce|mac`）、AES-CTR
  的 tail 机制都与 JS 参考实现逐字节对齐（见 `golden_test.go` 的向量）。

注意：bridge 页面假设节点在 **HTTPS** 之后（carrier 用 `wss://` 连回），所以
`TG_BRIDGE_HOST` 必须是能对外提供 HTTPS 的域名；裸 HTTP 监听只适合本地测试。

## Hysteria 2 入口（可选）

除 WebSocket 之外，还可以在同一进程里开一个 **Hysteria 2** 的 QUIC 入口，
让客户端按网络选择“WS/CDN 模式”或“QUIC 直连模式”：

```bash
# 端口固定 UDP 9500；HY2_LISTEN 只作为开关，值本身不参与监听地址。
# 接主控时不需要 -hy2-password：客户端用设备 UUID 认证（复用 VLESS 用户表）。
vless-small -uuid <UUID> -hy2-listen 1

# 正式证书（自签时省略 -hy2-cert/-hy2-key）
vless-small -uuid <UUID> -hy2-listen 1 \
  -hy2-cert /path/fullchain.pem -hy2-key /path/privkey.pem
```

未提供证书时会在二进制同目录的 `.certs/hy2/` 生成并复用一张自签证书
（CN/SAN 默认 `bing.com`，可用 `-hy2-domain` / `HY2_DOMAIN` 修改），
客户端需要设置 `tls.insecure: true`。证书 10 年有效，剩余不足 30 天或
域名变化时自动重新生成。

客户端配置（官方 Hysteria 客户端）：

```yaml
server: node.example.com:9500
auth: <设备 UUID>
tls:
  sni: bing.com
  insecure: true
socks5:
  listen: 127.0.0.1:1080
```

主控（`apps/web`）按同一组约定生成订阅：端口固定 9500、SNI 固定
`bing.com`、密码用设备 UUID、自签证书带 `insecure`。节点只上报一个
`hy2.enabled` 布尔值，表示入口有没有起来；端口可达性探测不通过时它会是
`false`，订阅里就不会出现这条直连节点。

实现范围（`internal/hy2/`）：

- HTTP/3 认证：`POST /auth` + `Hysteria-Auth`，成功返回 `233 HyOK`；
  认证失败返回 404，表现得像普通 Web 服务；
- TCP：QUIC 双向流 + `0x401` 帧 + 地址/padding，出站复用
  `app.DialOutbound`（同一套 DNS 缓存与并行拨号）；
- UDP：QUIC unreliable datagram，含 SessionID 映射、分片重组、空闲回收；
- 拥塞控制由客户端决定：认证响应返回 `Hysteria-CC-RX: auto`。

**未实现**：Brutal 拥塞控制、Salamander 混淆、masquerade、ACL、带宽限速。
需要这些特性时请直接用官方 Hysteria 服务端。

### 启动时的端口可达性探测

Hysteria 走 UDP，TCP 连通性测试对它没有意义。启动时会自动获取本机公网
IP，再通过 STUN 拿出口 NAT 映射地址，只有**映射端口与监听端口一致**才真正
启动 hy2 入口：

- 映射一致 → `hysteria2: port reachable, ...`，正常监听；
- 端口被 NAT 改写、只放行 443、CGNAT、或本机出站 UDP 不通 →
  `hysteria2: skip, ...`，节点只保留 WS 入口，避免客户端连上一个永远
  打不开的地址；
- 监听地址是回环、公网 IP 未知或不是公网地址 → 直接跳过，不发探测包。

探测最多等 3 秒（两个 STUN 服务器），整体上限 8 秒，失败不影响 WS 启动。
确认端口实际可达但探测被防火墙干扰时，用 `HY2_SKIP_PROBE=1` 跳过探测。

### 运行时重试

入口做成可启停的控制器（`internal/hy2/controller.go`）：探测失败后不会
每次上报都重试（那样每 60 秒产生一次 STUN 探测，也失去了“手动重试”的
意义）。需要重新尝试时，主控在节点列表里点“再次尝试开启 HY2”，下发递增的
`hy2.retry` 计数，节点收到后重新探测并绑定端口，不需要重启进程。

hy2 只在 `runtime=local` 时启用：托管容器/PaaS 的 UDP 端口通常不可公网
直达，主控也不会给这类节点下发开启信号。

注意：

- 需要公网 **UDP** 入口和有效 TLS 证书（自签证书只能配客户端
  `tls.insecure` 本地测试）；
- 不能放在 Cloudflare 代理后面——CDN 会终止 QUIC；
- 与 WS 入口共享同一份 UUID 用户表，但 Hysteria 客户端用密码认证，
  两者是独立的认证体系。

## 说明

- 基础依赖只有 `github.com/gorilla/websocket`；临时隧道默认使用纯 Go
  实现（`github.com/quic-go/quic-go`），`-tags cloudflared` 时改为内嵌
  官方 `github.com/cloudflare/cloudflared` 及其依赖。
- 每条连接首条 WS 消息是 VLESS 请求头，响应头为 `[0x00, 0x00]`。
  **响应头在认证通过后立即发出**，不等待目标域名解析和 TCP 建连，
  客户端可以更早开始发送请求数据；目标拨号失败时会话直接关闭。
- WS 读写缓冲和 TCP/UDP 转发缓冲均为 64 KiB，用内存换更少的系统调用
  和更平滑的吞吐（大消息不会再被 4 KiB 帧放大成多次写）。
- 支持 `cmd=1`（TCP）与 `cmd=2`（UDP，`[2B 长度][数据]` 分包）。
- 服务端每 25s 发一次 WS ping，客户端回 pong 即续期 60s 读超时。
- TLS 不内置；生产环境请放在反向代理后面，并记得给代理配置 Upgrade 头与较长的读超时。
