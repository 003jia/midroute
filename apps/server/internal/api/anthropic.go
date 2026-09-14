// Anthropic Messages 协议入口（MR-014）：/v1/messages、/v1/messages/count_tokens。
// 原生上游优先：请求体与 SSE 事件原样透传，保留 anthropic-version/anthropic-beta（allowlist）。
// 不支持的能力返回明确错误，不静默删字段。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"midroute/internal/connectors"
	"midroute/internal/domain"
	"midroute/internal/errs"
	"midroute/internal/httpserver"
	"midroute/internal/router"
)

// maxAnthropicBody 请求体上限（覆盖大上下文）。
const maxAnthropicBody = 8 << 20

// anthropicHeaderAllowlist 允许透传的请求头。
var anthropicHeaderAllowlist = []string{
	"anthropic-version",
	"anthropic-beta",
	"content-type",
	"x-api-key", // 由目标账户注入
}

// handleAnthropicMessages /v1/messages（MR-014）。
func (a *App) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAnthropicBody+1))
	if err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "读取请求体失败"))
		return
	}
	if len(body) > maxAnthropicBody {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体过大"))
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体必须是 JSON 对象"))
		return
	}
	if probe.Model == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 model"))
		return
	}
	if !a.checkModelAllowed(w, r, probe.Model) {
		return
	}
	requestID := "req_" + shortID()

	cands, _, err := a.Router.ResolveCandidates(r.Context(), probe.Model)
	if err != nil {
		a.logError(&router.Decision{RequestID: requestID, Alias: probe.Model}, err)
		httpserver.WriteError(w, a.mapRelayError(err))
		return
	}

	start := time.Now()
	var lastErr error
	for _, c := range cands {
		acc, err := a.Store.GetAccount(r.Context(), c.AccountID)
		if err != nil {
			lastErr = err
			continue
		}
		target, err := a.resolveTarget(r.Context(), acc, "")
		if err != nil {
			lastErr = err
			continue
		}
		upBody, err := rewriteAnthropicModel(body, c.ModelID)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "改写模型字段失败", err))
			return
		}
		ferr := a.forwardAnthropic(w, r, target, upBody, probe.Stream, requestID, acc, c.ModelID, start)
		if ferr == nil {
			return
		}
		lastErr = ferr
		if !connectors.Retryable(ferr) {
			break
		}
	}
	a.logError(&router.Decision{RequestID: requestID, Alias: probe.Model}, lastErr)
	if lastErr == nil {
		lastErr = router.ErrNoCandidate
	}
	httpserver.WriteError(w, a.mapRelayError(lastErr))
}

// forwardAnthropic 执行单次 Anthropic Messages 转发（原生透传 + 头保留 + 流式透传）。
func (a *App) forwardAnthropic(w http.ResponseWriter, r *http.Request, target *connectors.Target, body []byte, stream bool, requestID string, acc domain.Account, model string, start time.Time) error {
	hdr := map[string]string{"x-api-key": target.APIKey}
	for _, k := range anthropicHeaderAllowlist {
		if v := r.Header.Get(k); v != "" {
			hdr[k] = v
		}
	}
	path := "/v1/messages"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(target.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return connectors.Classify(0, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return connectors.ClassifyWithBody(resp.StatusCode, raw)
	}
	if !stream {
		// 非流式：透传响应体 + 记录用量（响应含 usage）
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		w.Header().Set("content-type", resp.Header.Get("content-type"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		a.recordAnthropicUsage(requestID, model, acc.ID, raw, time.Since(start).Milliseconds(), "")
		return nil
	}
	// 流式：SSE 逐事件透传
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming unsupported by client")
	}
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	return consumeAnthropicSSE(r.Context(), resp, func(b []byte) error {
		_, werr := w.Write(b)
		if werr == nil {
			flusher.Flush()
		}
		return werr
	})
}

// handleAnthropicCountTokens /v1/messages/count_tokens：上游原生计数优先；无账户返回明确错误。
func (a *App) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAnthropicBody+1))
	if err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "读取请求体失败"))
		return
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 model"))
		return
	}
	if !a.checkModelAllowed(w, r, probe.Model) {
		return
	}
	cands, _, err := a.Router.ResolveCandidates(r.Context(), probe.Model)
	if err != nil {
		httpserver.WriteError(w, a.mapRelayError(err))
		return
	}
	if len(cands) == 0 {
		httpserver.WriteError(w, errs.New(errs.CodeModelUnavailable, "没有可用渠道"))
		return
	}
	acc, err := a.Store.GetAccount(r.Context(), cands[0].AccountID)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	target, err := a.resolveTarget(r.Context(), acc, "")
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "解析目标失败", err))
		return
	}
	upBody, _ := rewriteAnthropicModel(body, cands[0].ModelID)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(target.BaseURL, "/")+"/v1/messages/count_tokens", bytes.NewReader(upBody))
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "构造请求失败", err))
		return
	}
	req.Header.Set("x-api-key", target.APIKey)
	req.Header.Set("anthropic-version", r.Header.Get("anthropic-version"))
	if v := r.Header.Get("anthropic-beta"); v != "" {
		req.Header.Set("anthropic-beta", v)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		httpserver.WriteError(w, a.mapRelayError(connectors.Classify(0, err)))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		httpserver.WriteError(w, a.mapRelayError(connectors.ClassifyWithBody(resp.StatusCode, raw)))
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	w.Header().Set("content-type", resp.Header.Get("content-type"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// rewriteAnthropicModel 仅替换 model 字段，其余字段原样保留（不静默删字段）。
func rewriteAnthropicModel(body []byte, model string) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	v["model"] = model
	return json.Marshal(v)
}

// recordAnthropicUsage 从响应提取 usage 并落用量（无正文）。
func (a *App) recordAnthropicUsage(requestID, model, accountID string, raw []byte, latencyMS int64, errCode errs.Code) {
	// 提取 usage 字段（input_tokens/output_tokens/cache_*），不解析正文
	var probe struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	var usageDTO *connectors.UsageDTO
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Usage != nil {
		cached := probe.Usage.CacheReadInputTokens + probe.Usage.CacheCreationInputTokens
		usageDTO = &connectors.UsageDTO{
			PromptTokens:        probe.Usage.InputTokens,
			CompletionTokens:    probe.Usage.OutputTokens,
			TotalTokens:         probe.Usage.InputTokens + probe.Usage.OutputTokens,
			PromptTokensDetails: &connectors.PromptTokensDetails{CachedTokens: cached},
		}
	}
	a.recordUsage(requestID, model, &router.Decision{RequestID: requestID, Alias: model, Selected: accountID + "@" + model}, usageDTO, latencyMS, errCode)
}

// consumeAnthropicSSE 流式 SSE 透传（ANTHROPIC 原生事件原样转发）。
func consumeAnthropicSSE(ctx context.Context, resp *http.Response, onChunk func([]byte) error) error {
	defer resp.Body.Close()
	buf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			// 按事件块（空行）切分转发
			for {
				idx := indexDoubleNewline(pending)
				if idx < 0 {
					break
				}
				event := pending[:idx]
				pending = pending[idx+2:]
				if len(event) > 0 {
					if err := onChunk(event); err != nil {
						return err
					}
				}
			}
		}
		if err == io.EOF {
			if len(pending) > 0 {
				return onChunk(pending)
			}
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func indexDoubleNewline(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
		if b[i] == '\n' && i+2 < len(b) && b[i+1] == '\r' && b[i+2] == '\n' {
			return i
		}
	}
	return -1
}
