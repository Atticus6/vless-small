# vless-small

最小可用的 **VLESS over WebSocket** 服务端，参考 [go-vless](https://github.com/Atticus6/go-vless) 精简而来。

只保留三件事：WebSocket 握手、VLESS 头解析 + UUID 认证、TCP/UDP 转发。
没有 TLS、隧道、管理后台和流量统计。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 启动流程、节点同步调度、优雅退出 |
| `config.go` | 命令行/环境变量解析、路径规范化 |
| `server.go` | 服务器状态、用户表、健康检查、WS 处理 |
| `websocket.go` | WebSocket 握手、TCP/UDP 转发、心跳 |
| `protocol.go` | VLESS 请求头解析（内存缓冲） |
| `node.go` | CONFIG 解析、节点上报、设备同步 |
| `push.go` | 主控新建设备后的即时设备列表推送 |
| `update.go` | 版本比较、自更新下载、sha256 校验与原子替换 |
| `tsnet_common.go` | tsnet 开关类型、hostname 清洗与控制器接口 |
| `tsnet_impl.go` | 非 Vercel 构建的 tsnet 实现：状态拉取/回写、tailnet 监听 |
| `tsnet_stub.go` | Vercel 构建的空实现（`-tags vercel`，不编译 tsnet） |
| `tunnel_common.go` | 临时隧道开关、上报结构与本地端口解析 |
| `tunnel_impl.go` | 非 Vercel 构建：内嵌 cloudflared，申请并运行临时隧道 |
| `tunnel_stub.go` | Vercel 构建的空实现（`-tags vercel`，不编译 cloudflared） |
| `cert_common.go` | 自动证书开关/状态与控制器接口 |
| `cert_impl.go` | Linux 非 Vercel：ACME 证书申请，监听 80/443 提供 TLS |
| `cert_stub.go` | Vercel/非 Linux 构建的空实现 |
| `platform.go` | 运行时识别、公网 hosts、IP/地理位置查询 |
| `users.go` | UUID 列表解析 |
| `constants.go` | 协议、超时与默认路径常量 |
| `version.txt` | 当前版本号，Release workflow 唯一版本来源 |
| `install.sh` | 一键安装脚本，作为 Release 资产发布 |
| `uninstall.sh` | 独立卸载脚本，作为 Release 资产发布 |
| `.github/workflows/release.yml` | push 触发的多平台构建与 GitHub Release |

## 运行

```bash
# 手动 UUID
UUID=14725836-1234-5678-9abc-def012345678 go run .

# 从主控同步设备（CONFIG = <baseURL>_<name>_<token>）
CONFIG=https://dsh.atticus.eu.org_hk_<token> go run .

# 等价写法
go run . -uuid 14725836-1234-5678-9abc-def012345678 -listen :8080 -path /ws
```

| 参数 / 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` / `PORT` | `:8080` | 监听地址；`PORT=9000` 时监听 `:9000` |
| `-uuid` / `UUID` | — | 逗号分隔的静态 UUID；设置 `CONFIG` 时可选 |
| `-path` / `WS_PATH` | 空（任意路径） | 只接受指定 WS 路径，如 `/ws` |
| `-config` / `CONFIG` | — | `<baseURL>_<name>_<token>`，用于向主控上报并同步设备 |
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

仓库是私有的。Web 控制台的“VLESS 管理”页会生成一条通过主控代理下载的
安装命令，节点上不需要 GitHub Token（Worker 用自己的 `GITHUB_TOKEN`
访问私有 Release）：

```bash
NODE_NAME='hk-01'; NODE_TOKEN='<node token>'; \
  curl -fsSL -H "Authorization: Bearer $NODE_TOKEN" -H "X-Node-Name: $NODE_NAME" \
    "https://dsh.example.com/api/releases/<version>/install.sh" \
  | sudo bash -s -- --config "https://dsh.example.com_hk-01_<node token>" \
      --version <version> \
      --download-base "https://dsh.example.com/api/releases/<version>" --path /ws
```

直接点页面上的“复制安装命令”即可，版本会自动取当前最新 Release。
如果直连 GitHub（公开 fork 或本地有可用 token），仍可使用
`--github-token` / `GITHUB_TOKEN`；私有仓库在部分网络下直链可能 404，
优先使用上面的主控代理。

脚本按 `uname` 选择对应资产并校验 sha256，创建 `vless-small` 系统用户，把配置
写入 `/etc/vless-small/vless-small.env`（0600），注册
`/etc/systemd/system/vless-small.service`，最后用 `http://127.0.0.1:<PORT>/healthz`
做启动检查。

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
| `--install-dir` | 二进制目录；root 默认 `/opt/vless-small`（服务用户可写，支持自更新），非 root 默认 `~/.local/bin`；root 安装会在 `/usr/local/bin` 建符号链接 |
| `--service` / `--no-service` | 强制或禁止注册 systemd 服务 |
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

服务管理：

```bash
systemctl status vless-small
journalctl -u vless-small -f
vless-small -version   # 查看已安装版本
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

脚本会停止/禁用并删除 systemd 服务、`/etc/vless-small`、`/opt/vless-small`
和 `/usr/local/bin/vless-small` 符号链接；`--docker` 额外删除名为
`vless-small` 的容器，`--keep-user` 保留服务用户，`--install-dir` 用于
非默认目录。`install.sh --uninstall` 仍然可用。

> 本服务不内置 TLS。生产环境请放在 Nginx / Caddy / Cloudflare 之后，并配置
> WebSocket Upgrade 与较长的读超时；订阅默认使用 443 + `/ws`。

## 自动更新（Web 下发）

节点每次上报都会带上 `version` / `os` / `arch`。主控“VLESS 管理”页点击
“下发更新”后，Worker 把最新 Release 的目标版本和原始二进制 sha256 写入节点
记录；节点在下一次上报（≤60 秒）收到更新指令后：

1. 用自身 `name` + `token` 从主控 `/api/nodes/update` 下载原始二进制
   （Worker 用 `GITHUB_TOKEN` 访问私有 Release，节点不接触 GitHub 凭据）；
2. 校验 sha256，在二进制同目录写临时文件并原子替换；
3. 退出，由 systemd / Docker 按重启策略拉起新版本。

**systemd**：新版安装脚本把二进制放到 `/opt/vless-small`（属主为服务用户），
`/usr/local/bin/vless-small` 是指向它的符号链接，systemd unit 通过
`ReadWritePaths=/opt/vless-small` 允许替换。已有旧节点需要**重跑一次**新版
`install.sh` 完成迁移，否则二进制仍位于 `/usr/local/bin`，服务用户没有写权限。

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
镜像，之后 Web 按钮才会生效。SaaS 自动休眠运行时（Vercel/Railway）不会后台
轮询，Web 会拒绝下发。

## Tailscale（tsnet）

非 Vercel 构建内置 [tsnet](https://tailscale.com/kb/1244/tsnet)，节点会作为一台
Tailscale 设备自动加入 tailnet。Vercel 构建使用 `-tags vercel`，完全不编译
tsnet（`vercel.json` 的 `buildCommand` 已加该 tag）。

自动加入条件（三条同时满足，主控每次上报重新判断）：

1. 节点二进制包含 tsnet（`tsnetSupported=true`，即非 Vercel 构建）；
2. 运行时长期驻留（Vercel / Railway 会自动休眠，不加入）；
3. Web 已配置 Tailscale API token（设置页），可签发 auth key。

满足后主控在 `/api/nodes/report` 响应里返回 `tsnet.enabled=true`；节点随即：

1. `GET /api/nodes/tsnet?name=...`（Bearer name+token）拉取 auth key、
   `tailscaled.state`、`tailscaled.log.conf`；
2. 把这些文件恢复到临时状态目录，以清洗后的节点名作为 hostname 启动 tsnet；
3. 在 tailnet 上监听 `:8080`，复用同一个 HTTP handler（健康检查/WS）；
4. 启动后和每 5 分钟把状态文件变化 `PUT /api/nodes/tsnet` 回写主控。

主控把状态存在 D1 的 `vless_tsnet_state` 表（base64），因此实例/容器重建后
仍复用同一台 Tailscale 设备，不会反复注册新设备。条件不再满足（例如删除
Tailscale API token）时，节点在下次上报后停止 tsnet；Tailscale 控制台里的
设备记录不会自动删除，需要手动清理。

> tsnet 使用用户态网络栈，tailnet IP 上的服务由内嵌 listener 提供，不会自动
> 暴露公网 `:8080`；公网入口仍走原有的 TLS 反向代理。

## 临时隧道（cloudflared）

非 Vercel 构建把 [cloudflared](https://github.com/cloudflare/cloudflared) 作为
Go 依赖直接编译进进程，节点会向 `api.trycloudflare.com` 申请一个 Quick Tunnel，
无需在机器上单独安装 cloudflared。

- 主控的“VLESS 管理 → 临时隧道”按节点开关，默认开启；节点在每次上报
  （≤60 秒）时读取 `tunnel.enabled`，启动/停止内置隧道。
- Quick Tunnel 地址形如 `https://random-words.trycloudflare.com`，
  **进程重启后会变化**；需要固定地址请改用 Cloudflare 命名隧道。
- 地址随 `/api/nodes/report` 上报，主控写入 `vless.tunnel_address`，列表和
  指标弹框可直接看到。
- 隧道固定回源到 `http://localhost:<PORT>`，复用同一个 HTTP handler（健康检查
  与 WebSocket）。
- 需要出站 UDP/TCP 7844；`auto` 协议会在 QUIC 与 HTTP/2 之间选择。
- Vercel 构建使用 `-tags vercel`，`tunnel_stub.go` 不导入 cloudflared，
  上报 `tunnel.supported=false`，主控不会下发启用指令。
- 内嵌 cloudflared 会让 Linux 二进制增大约 35 MiB（Vercel 构建仍是几 MiB），
  首次编译也会明显变慢。

> Quick Tunnel 仅适合临时/个人使用，Cloudflare 不提供可用性保证；生产环境
> 建议使用带固定域名的命名隧道。

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

- 仅 **纯 Linux 非 Docker** 环境；Docker/Podman/Kubernetes 会在上报中返回
  `cert.supported=false`。Vercel 构建（`-tags vercel`）完全剔除证书代码。
- 域名必须解析到该节点，且公网能访问 80/443。
- 安装脚本生成的 systemd 服务已加入 `CAP_NET_BIND_SERVICE`，服务用户可绑定
  低端口；手动运行需要 root 或同等 capability。
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
    "url": "https://random-words.trycloudflare.com"
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
4. 读取 `tunnel.enabled` 并启动/停止内置 cloudflared 临时隧道。

节点信息由 vless-small 自动收集：

| 字段 | 来源 |
| --- | --- |
| `version` | 构建时通过 `-ldflags -X main.version` 注入的 Release 版本；本地构建为 `dev` |
| `os` / `arch` | `runtime.GOOS` 与 Release 资产架构（仅发布 amd64/arm64） |
| `tsnetSupported` | 当前二进制是否包含 tsnet；Vercel 构建为 `false` |
| `tsnetIp` | 内嵌 Tailscale 连接成功后上报的 tailnet IPv4 |
| `cert` | 自动证书状态；主控据此写入 `cert_*` 并生成证书节点 |
| `runtime` | `NODE_RUNTIME` / `RUNTIME`，否则根据 `VERCEL`、`RAILWAY_*`、`RENDER_*`、`FLY_APP_NAME`、`KOYEB_*`、`CF_PAGES*`、`ZEABUR*`、`NF_*` 自动判断 |
| `hosts` | `PUBLIC_HOSTS`（逗号分隔）/ `PUBLIC_HOST`，否则读取平台分配的公网域名；Northflank 使用 `NF_HOSTS` |

只采集稳定的公网地址：Vercel 使用 `VERCEL_PROJECT_PRODUCTION_URL`（忽略每次部署变化的 `VERCEL_URL`）；
Cloudflare Pages 不使用 `CF_PAGES_URL`（单次部署地址），请通过 `PUBLIC_HOST` 手动指定生产域名。
| `ipv4` / `ipv6` | 本机公网 IP 查询；可用 `NODE_IPV4` / `NODE_IPV6` 覆盖 |
| `countryCode` / `latitude` / `longitude` | 本机 IP 查询结果 |

IP 查询默认请求 `https://ipinfo.io/json`，可用 `IP_LOOKUP_URL` 指向其他服务。
`name` 和 `token` 中不要包含下划线（`CONFIG` 使用下划线分隔）。

在 **Vercel / Railway** 上不会启动 60 秒后台同步，只在冷启动时同步一次，
避免空闲时持续请求让实例无法自动休眠。其他运行环境（包括 Northflank）仍按 60 秒刷新设备列表。

Northflank 会注入 `NF_HOSTS`（可能包含多个逗号分隔的地址），直接读取即可；
也可以用通用的 `PUBLIC_HOST` / `PUBLIC_HOSTS` 覆盖。

> 节点端始终拉取**全量启用设备 UUID**。Web 的用户/设备节点授权只影响订阅里
> 包含哪些节点，不会缩小节点端的 UUID 列表，因此授权变化不需要通知每个节点。

新建设备时主控会尝试 `POST <WS_PATH>`（Bearer CONFIG token）把完整设备列表
推给已上报公网地址的节点，让新 UUID 立即生效；Vercel 运行时是无状态实例，
主控会跳过（这类节点只在冷启动上报时同步）。节点仍按 60 秒周期拉取全量列表。
反向代理需要允许该 WS 路径上的 POST，推送失败不影响设备创建。

## 客户端

明文 WebSocket（或由 Nginx / Cloudflare 终止 TLS 后转发到本服务）：

```
vless://<DEVICE_ID>@<HOST>:<PORT>?encryption=none&security=none&type=ws&path=%2Fws#vless-small
```

`DEVICE_ID` 是主控中设备记录的 `id`（UUID）。走 TLS/WSS 时把 `security` 改成 `tls`，端口换成 443。

## 说明

- 基础依赖只有 `github.com/gorilla/websocket`；非 Vercel 构建额外包含
  `tailscale.com/tsnet` 和 `github.com/cloudflare/cloudflared`（临时隧道），
  Vercel 构建使用 `-tags vercel` 同时剔除两者。
- 每条连接首条 WS 消息是 VLESS 请求头，响应头为 `[0x00, 0x00]`。
- 支持 `cmd=1`（TCP）与 `cmd=2`（UDP，`[2B 长度][数据]` 分包）。
- 服务端每 25s 发一次 WS ping，客户端回 pong 即续期 60s 读超时。
- TLS 不内置；生产环境请放在反向代理后面，并记得给代理配置 Upgrade 头与较长的读超时。
