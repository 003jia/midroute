# 平台能力矩阵（MR-006）

> 更新：2026-09-07。证据基线：`PINNED_COMMITS` 冻结提交（CLIProxyAPI `17a65ee5`、CPA-Manager-Plus `be3039b6`、OneAPI `8df4a267`）。
> 判读规则：`supported` 只授予**有上游代码证据**的能力；未经真实账户验证的一律标注验证状态为 `unknown/待真实验收`。平台能力 ≠ 当前凭据权限。
> 状态语义见 [数据合约 v1](./contracts/data-contracts-v1.md) §capability。

## 1. 首批平台矩阵

| 平台 | 登录/OAuth | 模型发现 | 请求转发 | 订阅/窗口读取 | 验证状态 |
|---|---|---|---|---|---|
| Codex / ChatGPT | 代码证据 supported（授权码+PKCE，见 §2.1）；**真实登录待验收** | 待实现（Responses 目录） | 待实现（Responses 协议，MR-013） | 被动解析代码证据 supported（§2.3）；**真实窗口数值待验收** | unknown |
| Claude Code | 未核验（unknown） | 现有 connector 为静态目录 | OpenAI→Messages 转换已有（mock 验证） | 未核验（unknown） | unknown |
| Gemini CLI | 不适用（API Key） | supported（上游 `/v1beta/models` 发现，已有代码） | OpenAI→generateContent 转换已有（mock 验证） | 官方 Usage API unsupported（现有 `internal/usage/gemini.go` 明确返回） | 部分 mock 验证 |
| Kimi | 未核验（unknown） | unknown | 理论上 OpenAI-compatible（未验证） | unknown | unknown |
| GLM | 未核验（unknown） | unknown | 理论上 OpenAI-compatible（未验证） | unknown | unknown |
| 豆包 | 未核验（unknown） | unknown | 理论上 OpenAI-compatible（未验证） | unknown | unknown |
| 普通 API Key（OpenAI-compatible 通用） | 不适用 | supported（`GET /v1/models` 动态发现，已有代码+测试） | supported（chat/completions 非流式+流式，mock 验证） | 不适用 | mock 验证；真实账户待验收 |

## 2. Codex/ChatGPT 证据清单

来源仓库：`work/CLIProxyAPI`（冻结 `17a65ee5470fbaf0e22fc219381e6a4ae9e07624`，MIT）。

### 2.1 OAuth 登录/刷新
| 项 | 值/结论 | 证据路径 |
|---|---|---|
| 授权端点 | `https://auth.openai.com/oauth/authorize` | `internal/auth/codex/openai_auth.go:25` |
| 令牌端点 | `https://auth.openai.com/oauth/token` | 同文件 :26 |
| client_id | `app_EMoamEEZ73f0CkXaXp7hrann`（公共客户端） | 同文件 :27 |
| 回调 | `http://localhost:1455/auth/callback`（本机监听 1455） | 同文件 :28、`sdk/auth/codex.go:27` |
| PKCE | S256；verifier 96 字节随机 base64url | `internal/auth/codex/pkce.go` |
| scope | 授权 `openid email profile offline_access`；刷新 `openid profile email` | `openai_auth.go:76,218` |
| 刷新 | single-flight 单飞 + 30s 超时 + `context.WithoutCancel`；提前 5 天刷新（RefreshLead） | `openai_auth.go:39,198-211`、`sdk/auth/codex.go:34-36` |
| 刷新不可重试判定 | `refresh_token_reused` | `openai_auth.go:331-337` |
| 备选流程 | 设备流（`codex_device.go`/`shouldUseCodexDeviceFlow`）存在 | `sdk/auth/codex.go:49-51` |
| JWT | id_token 含 account_id/email claims | `internal/auth/codex/jwt_parser.go` |
| **Midroute 实测** | 未执行（需真实账户授权） | — |

Midroute 实现：`internal/connectors/oauth`（改写非复制，台账已登记）；刷新提前量取 5 分钟（工程默认，非平台官方限额）。

### 2.2 请求转发
| 项 | 值/结论 | 证据路径 |
|---|---|---|
| 推理端点 | `https://chatgpt.com/backend-api/codex/responses`（Responses 协议） | `internal/runtime/executor/codex_executor_execute.go:32,76` |
| compact 端点 | `.../responses/compact` | 同文件 :236 |
| 结论 | Codex 客户端兼容必须走 Responses 协议；不能用 Chat Completions 冒充 | 对应 MR-013 |

### 2.3 订阅/额度窗口
| 项 | 值/结论 | 证据路径 |
|---|---|---|
| 采集方式 | **被动观测**：额度信号随推理响应返回；无主动查询端点证据 | `sdk/cliproxy/auth/quota_signals.go:16-24,37` |
| 窗口头命名空间 | `x-codex-primary-*`、`x-codex-secondary-*`、`x-codex-additional-<名>-*`、`x-codex-code-review-*` | 同文件 :149-168 |
| 窗口字段 | `-used-percent`、`-window-minutes`、`-reset-after-seconds`、`-reset-at`、`-allowed`、`-limit-reached`、`-over-secondary-limit-percent` | 同文件 :200-214 |
| 套餐档位 | `x-codex-plan-type`、`x-codex-active-limit`、`x-codex-credits-*` | 同文件 :154,192-195 |
| 流内帧 | SSE 事件 `codex.rate_limits`（与 `codex.response.metadata` 并列） | `internal/runtime/executor/codex_executor_terminal.go:412-424` |
| 排除项 | 观测到的 Codex 响应**不带** `x-ratelimit-*` 头 | `quota_signals.go:180-183` 注释 |
| **窗口语义** | primary/secondary 的具体时长（社区普遍认为 5 小时/每周）**上游未声明，属未验证推断**；以实测 `-window-minutes` 为准 | — |
| **Midroute 实测** | 未执行；解析器 `internal/quota` 仅 fixture 验证 | — |

## 3. 与 Midroute 能力的映射

| Midroute 能力 | 依据本矩阵的结论 | 任务 |
|---|---|---|
| OAuth 登录/刷新/撤销 | Codex 授权码+PKCE 证据充分 → 实现；撤销无端点证据 → 仅本地断开 | MR-007 |
| 订阅/窗口读取 | 被动解析证据充分 → 实现响应头解析器；**主动查询不做**（无证据） | MR-008 |
| Responses 协议网关 | 端点证据充分，但协议细节待 MR-013 | MR-013 |
| Claude Code 接入 | 需另行核验 Messages/count_tokens/beta 头与网关文档 | 后续 |

## 4. 待真实验收项（不因本矩阵勾选）

1. Codex 真实账户完成一次浏览器授权（state/PKCE 全流程）。
2. 真实响应头/流内帧中 primary/secondary 的实际字段与时窗数值。
3. 刷新轮换在真实平台的保留/作废行为（本地 mock 已覆盖语义）。
4. Claude Code / Kimi / GLM / 豆包 全部能力核验。
