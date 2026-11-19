//go:build vercel

package tunnel

// Vercel 构建使用 `-tags vercel`，不编译 tunnel_impl.go，也不引入 cloudflared。
// 节点仍会上报 tunnel.supported=false，主控不会下发启用指令。
func Supported() bool { return false }

func New(string) Controller {
	return tunnelControllerStub{}
}

type tunnelControllerStub struct{}

func (tunnelControllerStub) Sync(bool) {}

func (tunnelControllerStub) Status() Report {
	return Report{Supported: false}
}

func (tunnelControllerStub) Close() {}
