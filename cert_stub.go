//go:build !linux || vercel

package main

import "net/http"

// 非 Linux 或 Vercel 构建不包含 ACME/证书代码。
func newCertController(http.Handler) certController {
	return certStub{}
}

type certStub struct{}

func (certStub) Sync(string) {}

func (certStub) SetReporter(func(certReport)) {}

func (certStub) Status() certReport {
	return certReport{Supported: false}
}

func (certStub) Close() {}
