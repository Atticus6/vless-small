# vless-small

最小可用的 **VLESS over WebSocket** 服务端，参考 [go-vless](https://github.com/Atticus6/go-vless) 精简而来。

只保留三件事：WebSocket 握手、VLESS 头解析 + UUID 认证、TCP/UDP 转发。
没有 TLS、隧道、管理后台和流量统计。

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
| `-xhttp-path` / `XHTTP_PATH` | `/xhttp` | XHTTP stream-one 路径；设为 `none` / `off` 可禁用 |
| `-config` / `CONFIG` | — | `<baseURL>_<name>_<token>`，用于向主控上报并同步设备 |

## XHTTP

XHTTP 默认启用，路径为 `/xhttp`，实现的是 **stream-one**：同一个 HTTP 请求同时承载上行和下行，
不实现 packet-up / stream-up 的上下行分流。请求体是 VLESS 字节流，响应体是下行字节流。

主控订阅会同时下发 WS 与 XHTTP 节点，节点默认路径与订阅里的 `/xhttp` 一致，因此无需额外配置：

```bash
CONFIG=... go run .
```

需要禁用 XHTTP 时设置 `XHTTP_PATH=none`；改路径则设置如 `XHTTP_PATH=/custom`，
并同步修改主控的节点路径（当前订阅固定为 `/xhttp`）。

Vercel Functions 会在调用前缓冲完整请求体（上限 4.5MB），无法承载 stream-one 的双向流式
请求；Vercel 部署请只使用 WS。XHTTP 需要 Northflank、Railway 等常驻容器/直接 TCP 环境。

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
| `runtime` | `NODE_RUNTIME` / `RUNTIME`，否则根据 `VERCEL`、`RAILWAY_*`、`RENDER_*`、`FLY_APP_NAME`、`KOYEB_*`、`CF_PAGES*`、`ZEABUR*`、`NF_*` 自动判断 |
| `hosts` | `PUBLIC_HOSTS`（逗号分隔）/ `PUBLIC_HOST`，否则读取平台分配的公网域名；Northflank 使用 `NF_HOSTS`（可逗号分隔多个） |

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
