//go:build vercel

package main

// Vercel 构建使用 `-tags vercel`，不编译 tunnel_impl.go，也不依赖 cloudflared。
// 节点仍会上报 tunnel.supported=false，主控不会下发启用指令。
func tunnelSupported() bool { return false }

func newTunnelController(string) tunnelController {
	return tunnelControllerStub{}
}

type tunnelControllerStub struct{}

func (tunnelControllerStub) Sync(bool) {}

func (tunnelControllerStub) Status() tunnelReport {
	return tunnelReport{Supported: false}
}

func (tunnelControllerStub) Close() {}
