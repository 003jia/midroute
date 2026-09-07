# 第三方代码来源台账（Source Ledger）

> 依据 PRD `tasks/prd-midroute-independent-platform.md` §7 与 §14-2。
> 原则：只直接移植许可证允许融合的代码；AGPL 不复制；每段移植代码必须登记来源。
> 本台账随移植工作持续更新；未登记来源的大段第三方代码不得进入 Midroute。

## 来源一：OneAPI（首要代码来源）

| 项 | 值 |
|---|---|
| 仓库 | https://github.com/songquanpeng/one-api |
| 许可证 | MIT |
| 本地克隆 | `work/one-api`（浅克隆） |
| 固定提交 | `8df4a2670b98266bd287c698243fff327d9748cf`（docs: update ByteDance Doubao model link in README） |
| 本地未提交改动 | `common/config/config.go`、`relay/adaptor/openai/token.go`（实验性，未纳入移植，待评审后丢弃或按移植规则登记） |
| 移植候选 | 渠道能力、令牌约束、模型映射、分发、中继适配器（§7） |
| 不移植 | 充值、兑换码、邀请、多租户销售逻辑 |

## 来源二：CLIProxyAPI

| 项 | 值 |
|---|---|
| 仓库 | https://github.com/router-for-me/CLIProxyAPI |
| 许可证 | MIT |
| 本地克隆 | `work/CLIProxyAPI`（浅克隆） |
| 固定提交 | `17a65ee5470fbaf0e22fc219381e6a4ae9e07624` |
| 移植候选 | Codex/Claude/Gemini CLI OAuth、Token 刷新、协议适配 |
| 不移植 | 独立服务生命周期、原配置体系、原管理接口 |

## 来源三：CPA-Manager-Plus

| 项 | 值 |
|---|---|
| 仓库 | https://github.com/seakee/CPA-Manager-Plus |
| 许可证 | MIT |
| 本地克隆 | `work/CPA-Manager-Plus`（浅克隆） |
| 固定提交 | `be3039b66917a13e91b51660ecc10c08dca4862a` |
| 移植候选 | 额度窗口解析、账户巡检、用量聚合、部分图表交互 |
| 不移植 | 独立 Manager Server、CPA 专属存储和登录体系 |

## 来源四：New API（仅参考，不复制）

| 项 | 值 |
|---|---|
| 仓库 | https://github.com/Calcium-Ion/new-api |
| 许可证 | AGPLv3 + 附加署名要求 |
| 本地克隆 | 未克隆（避免误复制风险） |
| 用途 | 公开行为、接口和产品设计参考 |
| 规则 | 默认不复制源码；如未来整体开源或取得商业许可，再单独评审 |

## 移植记录

> 每次移植按 PRD §7.1 七步执行，并在此追加一行：来源提交、原始路径 → Midroute 目标路径、改动说明、日期。

### 2026-09-04 · OneAPI 中继行为移植（M2）

- 来源提交：`8df4a26`（work/one-api）
- 原始路径（行为参考，非直接搬移）：`relay/adaptor/interface.go`、`relay/adaptor/anthropic/`、`relay/adaptor/gemini/`、`relay/adaptor/openai/`
- Midroute 目标：`apps/server/internal/connectors/`
- 改动说明：OneAPI 适配器深耦合 Gin + 全局 `meta.Meta`，未直接搬移。按其「OpenAI 统一入参 → 厂商协议转换 → 统一响应/SSE」的行为，用 Midroute 自有类型实现 Connector 合约（`Connector`）与 OpenAI-compatible / Anthropic / Gemini 三个连接器；新增超时、取消、脱敏与契约测试。
- 保留版权声明：OneAPI（MIT）行为参考已在 `THIRD_PARTY_NOTICES.md` 登记。

（后续移植逐项追加。）

### 2026-09-07 · CLIProxyAPI OAuth/额度参考移植（MR-007/008）

- 来源提交：`17a65ee5470fbaf0e22fc219381e6a4ae9e07624`（work/CLIProxyAPI）
- 原始路径（行为参考 + 小段改写，非整文件复制）：`internal/auth/codex/openai_auth.go`、`internal/auth/codex/pkce.go`、`sdk/cliproxy/auth/quota_signals.go`、`sdk/auth/codex.go`
- Midroute 目标：`apps/server/internal/connectors/oauth/`（PKCE/state/刷新单飞/撤销边界）、`apps/server/internal/quota/`（x-codex-* 响应头窗口解析）
- 改动说明：端点常量与流程语义（PKCE S256、scope、单飞刷新、WithoutCancel、x-codex-* 头命名空间）取自上游；实现按 Midroute 的 Vault/SecretRef、可注入时钟与 httptest 测试体系独立改写，未复制原文件。上游无公开撤销/主动查余额端点的结论一并继承（显式 unsupported）。
- 证据与核验：`docs/provider-capabilities.md` §2；fixture 测试 `oauth_test.go`、`codex_test.go`。
- 保留版权声明：CLIProxyAPI（MIT）已在 `THIRD_PARTY_NOTICES.md` 登记。

## 参考

- 许可证全文：`THIRD_PARTY_NOTICES.md`
- 运行数据与配置位置：`docs/data-inventory.md`