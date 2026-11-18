# vless-small

最小可用的 **VLESS over WebSocket** 服务端，参考 [go-vless](https://github.com/Atticus6/go-vless) 精简而来。

只保留三件事：WebSocket 握手、VLESS 头解析 + UUID 认证、TCP/UDP 转发。
没有 TLS、隧道、管理后台和流量统计。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 启动流程、节点同步调度、优雅退出 |
| `config.go` | 命令行/环境变量解析、路径规范化 |
| `server.go` | 服务器状态、用户表、健康检查、WS/XHTTP 分流 |
| `websocket.go` | WebSocket 握手、TCP/UDP 转发、心跳 |
| `xhttp.go` | XHTTP stream-one 请求与 TCP/UDP 字节流转发 |
| `xhttp_packet.go` | XHTTP packet-up 会话、上传队列与下载流 |
| `protocol.go` | VLESS 请求头解析（内存缓冲与流式两种） |
| `node.go` | CONFIG 解析、节点上报、设备同步 |
| `update.go` | 版本比较、自更新下载、sha256 校验与原子替换 |
| `platform.go` | 运行时识别、公网 hosts、IP/地理位置查询 |
| `users.go` | UUID 列表解析 |
| `constants.go` | 协议、超时与默认路径常量 |
| `version.txt` | 当前版本号，Release workflow 唯一版本来源 |
| `install.sh` | 一键安装脚本，作为 Release 资产发布 |
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
| `-xhttp-path` / `XHTTP_PATH` | `/xhttp` | XHTTP 路径；设为 `none` / `off` 可禁用 |
| `-config` / `CONFIG` | — | `<baseURL>_<name>_<token>`，用于向主控上报并同步设备 |
| `-version` | `dev` | 打印版本；Release 构建通过 `-ldflags -X main.version` 注入 tag |

## 一键安装

版本号以 `version.txt` 为准。每次 push（任意分支）都会触发 GitHub Actions
（`.github/workflows/release.yml`）：

1. 读取 `version.txt`，把最后一段数字 +1 作为本次版本号
   （`v0.1.0` → `v0.1.1`，`1` → `2`）；
2. 运行 `go vet` / `go test`，交叉编译 Linux 多架构二进制
   （amd64/arm64/armv6/armv7），生成 `checksums.txt` 并创建 GitHub Release；
3. 构建和发布成功后，把新版本号写回 `version.txt` 并推送到当前分支
   （提交信息带 `[skip ci]`，不会再次触发构建）。

不需要手动打 tag 或改版本号。如果要调整版本序列，直接修改 `version.txt`
并推送，workflow 会从该值继续 +1。

仓库是私有的，安装脚本和 Release 资产都需要一个有 `Contents: Read` 权限的
GitHub Token。Linux 上安装为 systemd 服务（需要 root）：

```bash
GITHUB_TOKEN='<github token>'; \
  curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
    https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
  | sudo GITHUB_TOKEN="$GITHUB_TOKEN" bash -s -- \
      --config "https://dsh.example.com_hk_<token>" --path /ws
```

也可以直接在控制台的“VLESS 管理”页点击“复制安装命令”，会自动填入当前节点
的 CONFIG 和 Web 配置的 `GITHUB_TOKEN`。

脚本按 `uname` 选择对应资产并校验 sha256，创建 `vless-small` 系统用户，把配置
写入 `/etc/vless-small/vless-small.env`（0600），注册
`/etc/systemd/system/vless-small.service`，最后用 `http://127.0.0.1:<PORT>/healthz`
做启动检查。

| 参数 | 说明 |
| --- | --- |
| `--config` / `--uuid` | 主控 CONFIG 或静态 UUID，至少提供一个 |
| `--port` | 监听端口，默认 8080 |
| `--path` | WebSocket 路径，如 `/ws`；不填则接受任意路径 |
| `--xhttp-path` | XHTTP 路径，默认 `/xhttp`；`none` 禁用 |
| `--public-host` / `--public-hosts` | 上报的公网域名，可重复或逗号分隔 |
| `--node-runtime` | 节点上报的 runtime，默认自动识别 |
| `--version` | 指定 Release 版本，默认 latest |
| `--github-token` | 私有仓库 token；也可用环境变量 `GITHUB_TOKEN` |
| `--install-dir` | 二进制目录；root 默认 `/opt/vless-small`（服务用户可写，支持自更新），非 root 默认 `~/.local/bin`；root 安装会在 `/usr/local/bin` 建符号链接 |
| `--service` / `--no-service` | 强制或禁止注册 systemd 服务 |
| `--uninstall` | 停止服务并删除配置与二进制 |

```bash
# 下面的示例省略了私有仓库的 token；请像上面一样给 curl 加 -H，
# 并通过 sudo GITHUB_TOKEN 传给安装脚本。

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

> 本服务不内置 TLS。生产环境请放在 Nginx / Caddy / Cloudflare 之后，并配置
> WebSocket Upgrade 与较长的读超时；订阅默认使用 443 + `/ws` + `/xhttp`。

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

## XHTTP

XHTTP 默认启用，路径为 `/xhttp`，支持两种模式：

- **stream-one**：`POST /xhttp`，请求体承载上行、响应体承载下行，适合直连或常驻容器。
- **packet-up**：`POST /xhttp/{session}/{seq}` 发送上传分片，`GET /xhttp/{session}` 接收下载流。
  每个 POST 都是完整请求，适合会缓冲请求体的 Serverless 平台（如 Vercel）。

packet-up 会话 30 秒内没有下载 GET 会被清理；上传队列最多缓冲 64 个乱序包、16 MiB。
主控订阅会按节点 runtime 自动选择模式：Vercel 使用 packet-up，其他节点使用 stream-one。

主控订阅会同时下发 WS 与 XHTTP 节点，节点默认路径与订阅里的 `/xhttp` 一致，因此无需额外配置：

```bash
CONFIG=... go run .
```

需要禁用 XHTTP 时设置 `XHTTP_PATH=none`；改路径则设置如 `XHTTP_PATH=/custom`，
并同步修改主控的节点路径（当前订阅固定为 `/xhttp`）。

Vercel Functions 会缓冲完整请求体，不能使用 stream-one；Serverless 平台请使用
packet-up 的 XHTTP 节点。packet-up 的下载 GET 需要持续流式响应，Vercel 已验证可用。

Xray 客户端示例（`network` 用 `xhttp` 或旧名 `splithttp`）：
```json
{
  "network": "xhttp",
  "xhttpSettings": {
    "path": "/xhttp",
    "mode": "stream-one"
  }
}
```

stream-one 需要端到端支持双向流式传输（直连 TLS/REALITY 或 HTTP/2）；经 Nginx/Cloudflare
时请使用 gRPC 透传配置，普通 HTTP/1.1 反向代理可能缓冲请求体导致失败。

## HTTP 健康检查

- `GET /` 返回 `vless-small ok`
- `GET /healthz` 返回实例存活时间：

```json
{"ok":true,"uptime":"1m23s","uptimeSeconds":83}
```

- WebSocket 路径（`WS_PATH`）仍要求 `Upgrade: websocket`，普通 HTTP 请求返回 426

## 主控同步

设置 `CONFIG` 后，节点启动时和每 60 秒会：

1. 向 `<baseURL>/api/nodes/report` 上报节点信息；
2. 使用 `Bearer <token>` 认证（`name` + `token` 必须与主控 `vless` 表中的记录一致）；
3. 接收主控返回的所有设备，把启用设备的 `id` 当作 VLESS UUID 接受连接。

节点信息由 vless-small 自动收集：

| 字段 | 来源 |
| --- | --- |
| `version` | 构建时通过 `-ldflags -X main.version` 注入的 Release 版本；本地构建为 `dev` |
| `os` / `arch` | `runtime.GOOS` 与 Release 资产架构（amd64/arm64/armv6/armv7，32 位 ARM 来自 `GOARM`） |
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

## 客户端

明文 WebSocket（或由 Nginx / Cloudflare 终止 TLS 后转发到本服务）：

```
vless://<DEVICE_ID>@<HOST>:<PORT>?encryption=none&security=none&type=ws&path=%2Fws#vless-small
```

`DEVICE_ID` 是主控中设备记录的 `id`（UUID）。走 TLS/WSS 时把 `security` 改成 `tls`，端口换成 443。

## 说明

- 依赖只有 `github.com/gorilla/websocket`。
- 每条连接首条 WS 消息是 VLESS 请求头，响应头为 `[0x00, 0x00]`。
- 支持 `cmd=1`（TCP）与 `cmd=2`（UDP，`[2B 长度][数据]` 分包）。
- 服务端每 25s 发一次 WS ping，客户端回 pong 即续期 60s 读超时。
- TLS 不内置；生产环境请放在反向代理后面，并记得给代理配置 Upgrade 头与较长的读超时。
