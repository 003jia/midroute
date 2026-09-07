package connectors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout 上游 HTTP 调用超时（FR-29）。
const defaultTimeout = 120 * time.Second

// httpDo 发送 JSON 请求并返回响应体（调用方负责 Close）。
func httpDo(ctx context.Context, client *http.Client, method, url string, hdr map[string]string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return client.Do(req)
}

// readJSON 解析 JSON 响应体到 out（调用方负责 Body 关闭，本函数关闭）。
func readJSON[T any](out *T, resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, redactInline(string(raw)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode upstream response: %w", err)
	}
	return nil
}

// readBytes 读取原始响应体。
func readBytes(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, redactInline(string(raw)))
	}
	return raw, nil
}

// consumeSSE 逐事件读取 SSE 流，回调原始事件字节（含 data: 前缀与换行分隔）。
// 返回末尾 usage（如有）。ctx 取消即中断。
func consumeSSE(ctx context.Context, resp *http.Response, onChunk func([]byte) error) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, redactInline(string(raw)))
	}
	reader := bufio.NewReader(resp.Body)
	var buf bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
		}
		// 事件以空行结束
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" && buf.Len() > 0 {
			if err := onChunk(buf.Bytes()); err != nil {
				return err
			}
			buf.Reset()
		}
		if err != nil {
			if err == io.EOF {
				if buf.Len() > 0 {
					return onChunk(buf.Bytes())
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// extractData 从 SSE 事件块中提取第一条 data: 载荷（事件可能含 event:/data: 多行）。
func extractData(block string) (string, bool) {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data: ")), true
		}
	}
	return "", false
}

// redactInline 对错误文本中的疑似凭据做占位（简单脱敏，完整走 credentials.Redactor）。
func redactInline(s string) string {
	const max = 300
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
