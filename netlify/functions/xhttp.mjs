// Netlify Functions 2.0：把 /xhttp/* 反向代理到真正的 vless-small 节点。
//
// Netlify Functions 不支持 WebSocket，也不能自己持有目标 TCP 连接；
// 因此这里只做纯 HTTP 流式转发，节点订阅使用 XHTTP packet-up。
// 需要设置 XHTTP_UPSTREAM，例如 https://p01--xxx.code.run。
export const config = {
  path: "/xhttp/*",
}

const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
])

export default async function handler(request) {
  const origin = (process.env.XHTTP_UPSTREAM || "").trim().replace(/\/+$/, "")
  if (!origin) {
    return new Response("XHTTP_UPSTREAM is not set\n", { status: 500 })
  }

  const incoming = new URL(request.url)
  const headers = new Headers(request.headers)

  // 逐跳头不能跨代理转发；accept-encoding 交给 fetch 自动处理，
  // 否则上游压缩响应被 fetch 解压后仍带着 content-encoding。
  for (const name of HOP_BY_HOP) {
    headers.delete(name)
  }
  headers.delete("host")
  headers.delete("accept-encoding")

  const init = {
    method: request.method,
    headers,
    redirect: "manual",
  }

  if (request.method !== "GET" && request.method !== "HEAD") {
    init.body = request.body
    // Node fetch 发送流式请求体时必须显式开启 half duplex。
    init.duplex = "half"
  }

  try {
    return await fetch(`${origin}${incoming.pathname}${incoming.search}`, init)
  } catch (error) {
    return new Response(`upstream error: ${error.message}\n`, { status: 502 })
  }
}
