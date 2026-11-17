// Netlify Server 入口（Node.js，Early Access）。
//
// Netlify Functions 的 Go 运行时既不能升级 WebSocket，也不能提供
// XHTTP 所需的流式请求/响应，所以这里把 Go 二进制作为子进程启动，
// Node 只负责原样转发 HTTP 请求和 WebSocket upgrade。
import { spawn } from "node:child_process"
import { chmodSync, copyFileSync, existsSync } from "node:fs"
import http from "node:http"
import net from "node:net"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

const here = path.dirname(fileURLToPath(import.meta.url))
const binaryName = process.platform === "win32" ? "vless-small.exe" : "vless-small"
const candidates = [
  path.join(here, binaryName),
  path.join(here, "..", "..", "netlify", "server", binaryName),
  path.join(process.cwd(), "netlify", "server", binaryName),
  path.join(process.cwd(), binaryName),
]
const sourceBinary = candidates.find((candidate) => existsSync(candidate))

if (!sourceBinary) {
  console.error(`[netlify] vless-small binary not found; tried:\n${candidates.join("\n")}`)
  process.exit(1)
}

// Netlify 的代码目录是只读的，且 bundle 可能不保留可执行位；
// 复制到 /tmp 并 chmod 后再启动。
const runtimeBinary = path.join(
  os.tmpdir(),
  `vless-small-${process.pid}${process.platform === "win32" ? ".exe" : ""}`,
)
copyFileSync(sourceBinary, runtimeBinary)
chmodSync(runtimeBinary, 0o755)

const internalPort = Number(process.env.INTERNAL_PORT || 18080)
const child = spawn(runtimeBinary, [], {
  env: {
    ...process.env,
    PORT: String(internalPort),
  },
  stdio: "inherit",
})
child.on("exit", (code, signal) => {
  console.error(`[netlify] vless-small exited code=${code} signal=${signal}`)
  process.exit(code ?? 1)
})

await waitForPort(internalPort, 15_000)

const server = http.createServer((req, res) => {
  const upstream = http.request(
    {
      host: "127.0.0.1",
      port: internalPort,
      method: req.method,
      path: req.url,
      headers: req.headers,
    },
    (upstreamRes) => {
      res.writeHead(upstreamRes.statusCode || 502, upstreamRes.headers)
      upstreamRes.pipe(res)
    },
  )

  upstream.on("error", (error) => {
    if (!res.headersSent) {
      res.writeHead(502, { "Content-Type": "text/plain; charset=utf-8" })
    }
    res.end(`bad gateway: ${error.message}\n`)
  })
  req.pipe(upstream)
})

// WebSocket / XHTTP upgrade 不能走 http.request，需要原始 TCP 透传。
server.on("upgrade", (req, socket, head) => {
  const upstream = net.connect(internalPort, "127.0.0.1", () => {
    let raw = `${req.method} ${req.url} HTTP/${req.httpVersion}\r\n`
    for (let i = 0; i < req.rawHeaders.length; i += 2) {
      raw += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`
    }
    raw += "\r\n"

    upstream.write(raw)
    if (head?.length) {
      upstream.write(head)
    }
    socket.pipe(upstream)
    upstream.pipe(socket)
  })

  const destroy = () => {
    socket.destroy()
    upstream.destroy()
  }
  socket.on("error", destroy)
  upstream.on("error", destroy)
  socket.on("close", () => upstream.destroy())
  upstream.on("close", () => socket.destroy())
})

// VLESS/XHTTP 会话是长连接；不要用 Node 默认请求超时切断。
server.requestTimeout = 0
server.headersTimeout = 0
server.timeout = 0

const listenPort = Number(process.env.PORT || 3000)
server.listen(listenPort, "0.0.0.0", () => {
  console.log(`[netlify] listening on :${listenPort}, upstream :${internalPort}`)
})

process.on("SIGTERM", () => {
  child.kill("SIGTERM")
})
process.on("SIGINT", () => {
  child.kill("SIGINT")
})

/** 等待 Go 子进程开始监听；冷启动时 Netlify 可能先于子进程可用。 */
function waitForPort(port, timeoutMs) {
  return new Promise((resolve, reject) => {
    const deadline = Date.now() + timeoutMs

    const attempt = () => {
      const socket = net.connect(port, "127.0.0.1")
      socket.once("connect", () => {
        socket.destroy()
        resolve()
      })
      socket.once("error", () => {
        socket.destroy()
        if (Date.now() >= deadline) {
          reject(new Error(`vless-small did not listen on :${port} within ${timeoutMs}ms`))
          return
        }
        setTimeout(attempt, 100)
      })
    }

    attempt()
  })
}
