// 定时向控制面上报 Netlify 节点，让订阅把它识别为 packet-up 节点。
// CONFIG 与控制面里 vless 表的 name/token 一致：
//   <baseURL>_<name>_<token>
export const config = {
  schedule: "@hourly",
}

export default async function report() {
  const raw = (process.env.CONFIG || "").trim()
  if (!raw) {
    return new Response("CONFIG is not set\n", { status: 500 })
  }

  const last = raw.lastIndexOf("_")
  const rest = last > 0 ? raw.slice(0, last) : ""
  const token = last > 0 ? raw.slice(last + 1).trim() : ""
  const sep = rest.lastIndexOf("_")
  const baseURL = sep > 0 ? rest.slice(0, sep).trim().replace(/\/+$/, "") : ""
  const name = sep > 0 ? rest.slice(sep + 1).trim() : ""

  if (!baseURL || !name || !token) {
    return new Response("invalid CONFIG\n", { status: 500 })
  }

  const siteURL = process.env.URL || process.env.DEPLOY_PRIME_URL || ""
  let host = ""
  try {
    host = new URL(siteURL).host
  } catch {
    return new Response("missing Netlify site URL\n", { status: 500 })
  }

  const response = await fetch(`${baseURL}/api/nodes/report`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({
      name,
      runtime: "netlify",
      hosts: [host],
    }),
  })

  if (!response.ok) {
    return new Response(`control plane returned ${response.status}\n`, { status: 502 })
  }
  return new Response("ok\n")
}
