package connectors

import "net/http"

// Options 连接器工厂选项。
type Options struct {
	Client *http.Client // 可注入（测试用）
}

// NewConnector 根据平台类型返回连接器。
func NewConnector(kind ProviderKind, opts Options) Connector {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	switch kind {
	case KindAnthropic:
		return NewAnthropicConnector(client)
	case KindGemini:
		return NewGeminiConnector(client)
	default:
		return NewOpenAICompatibleConnector(client, kind)
	}
}
