// Package accounts 提供账户能力评估（MR-004/006）。
// 原则：平台声明支持 ≠ 当前凭据权限足够；每个能力单独判定；
// 仅监测账户的转发/订阅等能力按模式限制返回；缺凭据/权限返回 permission_required 或 unknown，绝不伪造 supported。
package accounts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"midroute/internal/connectors"
	"midroute/internal/domain"
	"midroute/internal/errs"
	"midroute/internal/repository"
)

// ConnectorVersion 当前连接器版本（语义化）。
const ConnectorVersion = "0.1.0"

// CapabilityChecker 依赖集合。
type CapabilityChecker struct {
	Store *repository.Store
	// ResolveTarget 从账户解析可执行目标（含凭据解密；未解密成功返回 error）。
	ResolveTarget func(ctx context.Context, acc domain.Account) (*connectors.Target, error)
	// OAuthProviders 已注册 OAuth 的平台（provider kind）。
	OAuthProviders map[string]bool
	Now            func() string
}

// New 创建能力检查器。
func New(store *repository.Store, resolve func(context.Context, domain.Account) (*connectors.Target, error)) *CapabilityChecker {
	return &CapabilityChecker{
		Store:          store,
		ResolveTarget:  resolve,
		OAuthProviders: map[string]bool{},
		Now:            func() string { return time.Now().UTC().Format(time.RFC3339) },
	}
}

// RegisterOAuthProvider 登记支持 OAuth 的平台。
func (c *CapabilityChecker) RegisterOAuthProvider(kind string) { c.OAuthProviders[kind] = true }

func cap(name domain.CapabilityName, status domain.CapabilityStatus, reason string) domain.AccountCapability {
	return domain.AccountCapability{
		Capability:       name,
		Status:           status,
		Reason:           reason,
		ConnectorVersion: ConnectorVersion,
		CheckedAt:        time.Now().UTC().Format(time.RFC3339),
	}
}

// Check 评估账户的全部能力并落库返回。
func (c *CapabilityChecker) Check(ctx context.Context, acc domain.Account) ([]domain.AccountCapability, error) {
	prov, err := c.Store.GetProvider(ctx, acc.ProviderID)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, "查询 Provider 失败", err)
	}

	// 目标解析（凭据可用性）
	var target *connectors.Target
	var resolveErr error
	if acc.AuthType != string(domain.AuthManual) {
		target, resolveErr = c.ResolveTarget(ctx, acc)
	}
	credsOK := resolveErr == nil && target != nil

	kind := connectors.ProviderKind(prov.Kind)
	conn := connectors.NewConnector(kind, connectors.Options{})

	results := make([]domain.AccountCapability, 0, 8)

	// 1) verifyCredential
	if acc.AuthType == string(domain.AuthManual) {
		results = append(results, cap(domain.CapVerifyCredential, domain.CapabilityUnsupported, "手工数据，无在线凭据可验证"))
	} else if !credsOK {
		results = append(results, cap(domain.CapVerifyCredential, domain.CapabilityError, "凭据解析失败：请检查凭据是否仍存在于密钥库"))
	} else {
		if err := conn.ValidateCredential(ctx, *target); err == nil {
			results = append(results, cap(domain.CapVerifyCredential, domain.CapabilitySupported, ""))
		} else {
			results = append(results, cap(domain.CapVerifyCredential, statusFromErr(err), "凭据校验失败"))
		}
	}

	// 2) discoverModels
	if !credsOK {
		results = append(results, cap(domain.CapDiscoverModels, domain.CapabilityError, "凭据不可用"))
	} else {
		infos, err := conn.DiscoverModels(ctx, *target)
		if err == nil {
			status := domain.CapabilitySupported
			reason := fmt.Sprintf("发现 %d 个模型", len(infos))
			// 静态目录（Anthropic）仅代表目录可用，不代表实时权限
			if len(infos) == len(staticCatalogFor(kind)) && len(infos) > 0 {
				status = domain.CapabilityUnknown
				reason = "模型来自静态目录，未核验实时列表权限"
			}
			results = append(results, cap(domain.CapDiscoverModels, status, reason))
		} else {
			results = append(results, cap(domain.CapDiscoverModels, statusFromErr(err), "模型发现失败"))
		}
	}

	// 3) forward —— 仅转发并监测账户具备
	if acc.Mode == string(domain.ModeMonitorOnly) {
		results = append(results, cap(domain.CapForward, domain.CapabilityUnsupported, "仅监测账户不参与请求转发"))
	} else if !credsOK {
		results = append(results, cap(domain.CapForward, domain.CapabilityError, "凭据不可用"))
	} else if acc.AuthState == string(domain.AuthStateReauthRequired) || acc.AuthState == string(domain.AuthStateExpired) {
		results = append(results, cap(domain.CapForward, domain.CapabilityReauthRequired, "认证已过期，需重新授权"))
	} else {
		results = append(results, cap(domain.CapForward, domain.CapabilitySupported, ""))
	}

	// 4) oauth —— 取决于平台与认证类型
	if c.OAuthProviders[prov.Kind] {
		switch acc.AuthType {
		case string(domain.AuthOAuth):
			if acc.AuthState == string(domain.AuthStateReauthRequired) {
				results = append(results, cap(domain.CapOAuth, domain.CapabilityReauthRequired, "需要重新授权"))
			} else {
				results = append(results, cap(domain.CapOAuth, domain.CapabilitySupported, ""))
			}
		case string(domain.AuthAPIKey):
			results = append(results, cap(domain.CapOAuth, domain.CapabilityUnsupported, "当前使用 API Key，无 OAuth 会话"))
		default:
			results = append(results, cap(domain.CapOAuth, domain.CapabilityUnknown, "未配置 OAuth 或 API Key"))
		}
	} else {
		results = append(results, cap(domain.CapOAuth, domain.CapabilityUnsupported, "该平台未提供已验证的 OAuth 接入"))
	}

	// 5) subscription / 6) quota —— 取决于平台能力声明与认证方式
	subStatus, subReason := subscriptionCapability(prov.Kind, acc.AuthType)
	results = append(results, cap(domain.CapSubscription, subStatus, subReason))
	quotaStatus, quotaReason := quotaCapability(prov.Kind, acc.AuthType)
	results = append(results, cap(domain.CapQuota, quotaStatus, quotaReason))

	// 7) refresh —— OAuth 有刷新路径；API Key 无
	if acc.AuthType == string(domain.AuthOAuth) {
		results = append(results, cap(domain.CapRefresh, domain.CapabilitySupported, "OAuth 令牌刷新（单飞+凭据版本比较）"))
	} else if acc.AuthType == string(domain.AuthAPIKey) {
		results = append(results, cap(domain.CapRefresh, domain.CapabilityUnsupported, "API Key 无需刷新"))
	} else {
		results = append(results, cap(domain.CapRefresh, domain.CapabilityUnsupported, "手工数据无刷新"))
	}

	// 8) probeHealth
	if !credsOK {
		results = append(results, cap(domain.CapProbeHealth, domain.CapabilityError, "凭据不可用"))
	} else {
		results = append(results, cap(domain.CapProbeHealth, domain.CapabilitySupported, "主动健康探测（受预算与频率约束）"))
	}

	if err := c.Store.SaveAccountCapabilities(ctx, acc.ID, results); err != nil {
		return results, errs.Wrap(errs.CodeInternal, "保存能力矩阵失败", err)
	}
	return results, nil
}

func statusFromErr(err error) domain.CapabilityStatus {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "403"), strings.Contains(msg, "权限"):
		return domain.CapabilityPermissionRequired
	case strings.Contains(msg, "429"), strings.Contains(msg, "rate"):
		return domain.CapabilityUnknown
	default:
		return domain.CapabilityError
	}
}

func subscriptionCapability(kind, authType string) (domain.CapabilityStatus, string) {
	switch kind {
	case "codex":
		return domain.CapabilityUnknown, "Codex 订阅采用被动响应头观测；真实窗口待验收"
	case "anthropic":
		return domain.CapabilityPermissionRequired, "需要组织级 Admin 凭据或已验证的订阅读取能力"
	case "gemini":
		return domain.CapabilityUnsupported, "官方订阅读取需 GCP Billing 权限，未提供"
	default:
		if authType == string(domain.AuthOAuth) {
			return domain.CapabilityUnknown, "订阅读取能力待该平台验证"
		}
		return domain.CapabilityUnsupported, "该平台无已验证的订阅读取方式"
	}
}

func quotaCapability(kind, authType string) (domain.CapabilityStatus, string) {
	switch kind {
	case "codex":
		return domain.CapabilityUnknown, "Codex 额度来自响应头被动观测；真实数值待验收"
	case "gemini":
		return domain.CapabilityPermissionRequired, "官方配额读取需 Google Cloud Monitoring/Quota 权限"
	case "anthropic":
		return domain.CapabilityPermissionRequired, "组织级用量/额度需 Admin 凭据"
	default:
		return domain.CapabilityUnsupported, "无已验证的官方额度读取方式"
	}
}

func staticCatalogFor(kind connectors.ProviderKind) []string {
	if kind == connectors.KindAnthropic {
		return []string{
			"claude-3-5-haiku-20241022", "claude-3-5-sonnet-20241022", "claude-3-5-sonnet-20240620",
			"claude-sonnet-4-20250514", "claude-opus-4-20250514", "claude-3-opus-20240229", "claude-3-haiku-20240307",
		}
	}
	return nil
}