package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CallbackResult 是本机回调监听收到的一次授权回调。
type CallbackResult struct {
	Code  string
	State string
	Error string
}

// AwaitCallback 在给定 listener 上等待平台重定向回调，只读取
// code/state/error 三个查询参数；收到一个回调即优雅关闭并返回。
// 生产用法：net.Listen("tcp", "127.0.0.1:1455")（与 Codex RedirectURI 端口一致）。
// 超时由 ctx 或 timeout 控制。
func AwaitCallback(ctx context.Context, ln net.Listener, timeout time.Duration) (CallbackResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	resultCh := make(chan CallbackResult, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := CallbackResult{Code: q.Get("code"), State: q.Get("state"), Error: q.Get("error")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if res.Error == "" && res.Code != "" {
			fmt.Fprint(w, "<!doctype html><meta charset=\"utf-8\"><title>Midroute</title><p>授权完成，请返回 Midroute 继续。</p>")
		} else {
			fmt.Fprint(w, "<!doctype html><meta charset=\"utf-8\"><title>Midroute</title><p>授权未完成，请返回 Midroute 重试。</p>")
		}
		resultCh <- res
	})}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// 优雅关闭：等待在途响应写回，避免客户端收到 EOF
		_ = srv.Shutdown(sctx)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-resultCh:
		shutdown()
		return res, nil
	case <-ctx.Done():
		shutdown()
		return CallbackResult{}, ctx.Err()
	case <-timer.C:
		shutdown()
		return CallbackResult{}, fmt.Errorf("oauth: callback timeout after %s", timeout)
	case err := <-served:
		return CallbackResult{}, fmt.Errorf("oauth: callback listener stopped: %w", err)
	}
}

// ExtractCallback 从一个完整回调 URL 中提取 code/state/error，
// 供用户手动粘贴回调地址的场景使用。
func ExtractCallback(raw string) (CallbackResult, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return CallbackResult{}, fmt.Errorf("oauth: parse callback url: %w", err)
	}
	q := u.Query()
	return CallbackResult{Code: q.Get("code"), State: q.Get("state"), Error: q.Get("error")}, nil
}
