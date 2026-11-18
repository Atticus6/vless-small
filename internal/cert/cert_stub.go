//go:build !linux || vercel

package cert

import "net/http"

// 非 Linux 或 Vercel 构建不包含 ACME/证书代码。
func New(http.Handler) Controller {
	return certStub{}
}

type certStub struct{}

func (certStub) Sync(string) {}

func (certStub) SetReporter(func(Report)) {}

func (certStub) Status() Report {
	return Report{Supported: false}
}

func (certStub) Close() {}
