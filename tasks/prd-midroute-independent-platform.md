# PRD：Midroute 自主中间路由平台

> 状态：产品方向已确认；实现方法、执行步骤与编码规范已补充，功能按验收清单推进  
> 创建日期：2026-09-04；更新日期：2026-09-07  
> 当前生效方向：自主开发主工程，选择性移植 MIT 开源项目中的接入代码  
> 被替代方案：三个上游服务并行运行并通过端口串联

执行入口：[实施步骤与验收清单](./midroute-implementation-checklist.md)。本文保留产品范围和 M0–M7 编号；任务依赖、开发顺序、接口细节、编码规范和完成证据以执行清单为准。未勾选不等于没有代码，只有完整验收后才勾选。

## 1. 项目概述

Midroute 是一个本地优先的 AI 接入、凭据、套餐额度、用量与路由管理平台。项目拥有自己的后端、前端、数据库、领域模型和发布产物，不以 OneAPI、CLIProxyAPI 或 CPA-Manager-Plus 作为运行依赖。

现有开源项目只作为代码来源和行为参考：把已经验证过的 Provider 接入、协议转换、OAuth、额度解析、模型映射、健康检测等代码按模块移植进 Midroute，随后统一改造成 Midroute 的接口、数据模型和测试。

最终用户只启动一个 Midroute 服务，使用一个管理界面、一个数据库和一个 API 地址。

### 1.1 已确认决策

- Midroute 是独立产品，不 fork 某个上游后直接改名发布。
- OneAPI 是首要代码来源，优先移植渠道、令牌、模型能力和中继逻辑。
- CLIProxyAPI 是 Coding Plan、Codex、Claude Code、Gemini CLI OAuth 与协议适配代码来源。
- CPA-Manager-Plus 是额度窗口、账户巡检、用量分析和观测界面代码来源。
- 三个项目不再作为三个常驻服务运行。
- 默认按未来可闭源的边界设计；只直接移植许可证允许融合的代码。
- New API 的 AGPL 源码不直接复制；如未来决定整体开源或取得商业许可，再单独评审。

### 1.2 当前假设

- [Assumption] V0.1 主要服务单个本机管理员，不实现公开注册、充值和 API 分销市场。
- [Assumption] 后端继续使用 Go，前端使用 React + TypeScript。
- [Assumption] V0.1 使用 SQLite，数据访问层为后续 PostgreSQL 预留迁移能力。
- [Assumption] macOS 是首个交付平台，Linux/Docker 在核心功能稳定后补齐。
- [Assumption] 本地模式不要求每次输入管理员密钥；远程访问必须启用明确认证。

## 2. 产品目标

- 只配置一次平台账号、API Key 或 Coding Plan，之后在一个界面查看模型、额度和状态。
- 提供 Chat Completions、Responses 和 Anthropic Messages 接入路径；按客户端兼容矩阵声明支持范围。
- 展示 API 套餐余额、Token 消耗、本地观测用量及可获取的官方用量。
- 展示 Coding Plan 的 5 小时窗口、一周窗口、重置时间和数据可信度。
- 基于真实延迟、TTFT、429、5xx 和超时计算模型拥挤度与健康状态。
- 支持按优先级、权重、余额、健康度和失败状态选择渠道。
- 凭据不进入日志、普通数据库字段、导出文件或 Git 仓库。
- 保留所有移植代码的来源、提交号、许可证和本地修改记录。

## 3. 非目标

- V0.1 不提供公开用户注册、邀请奖励、充值、兑换码和支付系统。
- V0.1 不做 API 转售市场或多租户商业计费平台。
- V0.1 不承诺所有 Coding Plan 都有官方公开额度 API。
- V0.1 不通过抓取网页、绕过验证码或违反平台条款的方式获取额度。
- V0.1 不保存用户 Prompt、模型回答或文件正文。
- V0.1 不同时维护 OneAPI、CLIProxyAPI 和 CPA-Manager-Plus 三套数据库与登录体系。
- 未经过许可证评审，不直接复制 AGPL、商业许可或来源不明代码。

## 4. 用户故事

### US-001：创建自主工程骨架

**Description:** 作为开发者，我希望 Midroute 有独立的工程入口和模块边界，以便后续移植代码时不会继续依赖原项目运行。

**Acceptance Criteria:**

- [ ] 存在唯一后端入口和唯一前端入口。
- [ ] `./build/test.sh` 可以从仓库根执行并覆盖 `apps/server` 的全部 Go 测试；原根目录裸 `go test ./...` 条目按现有子模块结构修正。
- [ ] 前端 typecheck、lint 和 build 可从仓库根执行。
- [ ] 构建产物不包含 `cli-proxy-api`、`cpa-manager-server` 或独立 `one-api` 可执行文件。
- [ ] 旧上游代码只存在于明确标识的只读参考目录中。

### US-002：建立第三方代码移植台账

**Description:** 作为维护者，我希望知道每段移植代码来自哪里，以便满足许可证要求并安全同步上游修复。

**Acceptance Criteria:**

- [ ] 每个来源记录仓库 URL、提交 SHA、许可证和原始文件路径。
- [ ] 每个移植模块记录 Midroute 目标路径和修改说明。
- [ ] 所有直接移植的 MIT 代码保留原版权与许可声明。
- [ ] CI 能拒绝未登记来源的大段第三方代码。
- [ ] New API 源码默认标记为“仅参考，不复制”。

### US-003：管理 API Key Provider

**Description:** 作为管理员，我希望添加不同厂商的 API Key，以便统一发现模型并通过 Midroute 调用。

**Acceptance Criteria:**

- [ ] 支持创建、编辑、禁用和删除 Provider Account。
- [ ] 保存前验证必填字段，但不会在错误中回显完整密钥。
- [ ] 至少支持 OpenAI、Anthropic、Gemini 和一个 OpenAI-compatible Provider。
- [ ] 能刷新并持久化每个账户可用模型列表。
- [ ] API Key 只通过 SecretRef 引用，不以明文写入业务表。
- [ ] 前端在浏览器中完成添加、编辑和禁用流程验证。

### US-004：接入 Coding Plan

**Description:** 作为管理员，我希望接入 Coding Plan 或 OAuth 账户，以便统一管理订阅模型与套餐限额。

**Acceptance Criteria:**

- [ ] Connector 接口支持 OAuth 登录、Token 刷新和撤销。
- [ ] 首批实现 Codex/ChatGPT Subscription Connector。
- [ ] 后续 Connector 可以复用同一接口接入 Claude Code、Kimi、GLM 或豆包 Coding Plan。
- [ ] Token 刷新失败时保留旧凭据并产生可诊断事件。
- [ ] OAuth 回调、state 和 PKCE 校验有自动化测试。
- [ ] 前端在浏览器中验证连接状态、失效状态和重新授权入口。

### US-005：查看套餐与额度窗口

**Description:** 作为管理员，我希望查看套餐、剩余额度和重置时间，以便判断何时切换账户或模型。

**Acceptance Criteria:**

- [ ] 统一展示总额度、已用额度、剩余额度和重置时间。
- [ ] 支持表达 5 小时、一周、自然日和账单周期窗口。
- [ ] 每项额度分别标记来源、可信度、可用状态与新鲜度；字段枚举和空值语义遵循执行清单 §3.2。
- [ ] 官方数据与本地观测数据分列显示，不进行无依据求和。
- [ ] 无稳定读取方式的平台明确显示“暂不可自动读取”。
- [ ] 历史快照可以按账户和窗口查询。
- [ ] 前端在浏览器中验证正常、过期、不可用和估算状态。

### US-006：统一请求转发

**Description:** 作为 API 使用者，我希望只配置一个 Base URL 和 Token，以便调用不同平台的模型。

**Acceptance Criteria:**

- [ ] 提供 `/v1/models` 和 `/v1/chat/completions`。
- [ ] 支持流式与非流式转发。
- [ ] 每次请求记录 Provider、Account、Model、Token、延迟和错误分类元数据。
- [ ] 请求日志不保存 Prompt 和模型回答正文。
- [ ] 客户端取消请求后，上游请求同步取消。
- [ ] 上游超时、限额、鉴权和服务错误映射为稳定错误代码。

### US-007：模型映射与路由策略

**Description:** 作为管理员，我希望配置模型别名和渠道优先级，以便在多个账户之间自动选择合适路线。

**Acceptance Criteria:**

- [ ] 一个逻辑模型可以映射到多个 Provider Model。
- [ ] 支持优先级、权重和手动启停。
- [ ] 账户无额度、认证失效或熔断时不会被选中。
- [ ] 自动重试只发生在可安全重试的失败类型上。
- [ ] 非幂等请求不会因盲目重试产生重复副作用。
- [ ] 路由决策写入审计事件，但不包含密钥。

### US-008：模型健康与拥挤度

**Description:** 作为管理员，我希望看到模型是否拥挤以及判断依据，以便主动切换路线。

**Acceptance Criteria:**

- [ ] 采集连接耗时、TTFT、总延迟、429、5xx、超时和样本数。
- [ ] 输出 0–100 健康分和 `healthy`、`busy`、`degraded`、`unavailable` 状态；无样本或样本不足时明确 `unknown`。
- [ ] 拥挤度结论包含置信度和统计窗口。
- [ ] 小样本不会触发自动切换。
- [ ] 使用滞回避免状态频繁抖动。
- [ ] 健康探测设置超时、并发上限和退避策略。
- [ ] 前端在浏览器中验证状态、趋势和低置信度提示。

### US-009：Token 与费用统计

**Description:** 作为管理员，我希望查看各模型和账户的 Token 消耗与费用，以便控制成本。

**Acceptance Criteria:**

- [ ] 分别记录 input、output、cache 和 reasoning Token（上游可提供时）。
- [ ] 费用计算绑定价格版本和生效时间。
- [ ] 支持按小时、日、周、账户、Provider 和模型聚合。
- [ ] 官方账单与本地估算能够对账并显示差值。
- [ ] 缺失价格时显示不可估算，不默认为零成本。
- [ ] 前端在浏览器中验证筛选、聚合和对账展示。

### US-010：安全的本地登录与凭据存储

**Description:** 作为本机用户，我希望日常打开管理台不重复输入管理员密钥，同时远程访问仍然安全。

**Acceptance Criteria:**

- [ ] 本地免重复输入模式只接受 loopback 来源。
- [ ] 远程监听时必须配置用户认证或明确拒绝启动。
- [ ] macOS 使用 Keychain 保存敏感凭据。
- [ ] API、日志和导出功能统一执行脱敏。
- [ ] Secret 读取、使用和释放路径有单元测试。
- [ ] 前端在浏览器中验证本地进入、远程拒绝和退出流程。

### US-011：备份、迁移与审计

**Description:** 作为维护者，我希望安全升级和迁移数据，以便二次开发不会破坏已有配置与历史用量。

**Acceptance Criteria:**

- [ ] 数据库 schema 使用单向版本化 migration。
- [ ] 升级前可以生成不含明文密钥的备份。
- [ ] migration 失败时事务回滚或拒绝启动。
- [ ] 可从当前 OneAPI SQLite 导入渠道和模型映射，但不直接覆盖 Midroute 数据。
- [ ] 导入提供 dry-run、冲突报告和幂等测试。
- [ ] 关键管理操作产生审计记录。

## 5. 功能需求

- FR-1：系统必须以单个 Midroute 服务提供管理 API、管理页面和模型中继。
- FR-2：系统必须通过统一 Connector 接口封装不同 Provider。
- FR-3：Connector 必须实现凭据验证能力或明确返回不支持。
- FR-4：Connector 必须实现模型发现能力或使用带版本的静态目录。
- FR-5：参与模型路由的 Connector 必须实现请求转发能力；仅监测连接器不要求实现转发。
- FR-6：支持 OAuth 的 Connector 必须实现安全的 Token 刷新。
- FR-7：支持额度读取的 Connector 必须返回标准化 Quota Window。
- FR-8：系统必须区分官方额度、本地观测、估算和不可用数据。
- FR-9：系统必须支持 OpenAI-compatible 模型列表接口。
- FR-10：系统必须支持 OpenAI-compatible Chat Completions 接口。
- FR-11：系统必须对流式连接传播客户端取消信号。
- FR-12：系统必须记录请求用量元数据。
- FR-13：系统不得默认记录 Prompt 或响应正文。
- FR-14：系统必须支持模型别名。
- FR-15：系统必须支持渠道优先级。
- FR-16：系统必须支持同优先级渠道权重。
- FR-17：系统必须支持基于账户额度排除渠道。
- FR-18：系统必须支持基于健康状态熔断渠道。
- FR-19：系统必须计算带置信度的健康和拥挤度状态。
- FR-20：系统必须保存额度与健康历史快照。
- FR-21：系统必须按价格版本计算估算费用。
- FR-22：系统必须将凭据保存为 SecretRef。
- FR-23：系统必须在日志、错误和导出中脱敏凭据。
- FR-24：系统必须限制本地免登录模式只能从 loopback 使用。
- FR-25：系统必须对数据库变更执行版本化 migration。
- FR-26：系统必须提供数据导入 dry-run。
- FR-27：系统必须保留第三方代码来源和许可证台账。
- FR-28：系统必须提供 `/healthz` 和 `/readyz`。
- FR-29：系统必须为外部 HTTP 调用设置超时和取消机制。
- FR-30：系统必须为后台刷新任务设置并发上限和退避机制。
- FR-31：系统必须允许账户选择仅监测或参与路由模式。
- FR-32：系统必须展示当前凭据逐项能力及不支持的原因。
- FR-33：系统必须为不可自动读取的套餐信息提供带来源标记的手工补充入口。
- FR-34：系统必须提供声明兼容范围的 Responses 接口。
- FR-35：系统必须提供声明兼容范围的 Anthropic Messages 接口。
- FR-36：系统必须提供与支持模型相匹配的 Token counting 能力或明确的不支持结果。
- FR-37：系统必须通过共享额度池避免多个 Key 重复统计同一份额度。
- FR-38：系统必须区分额度窗口类型和计量单位。
- FR-39：系统必须在未明确启用付费回退时禁止从订阅切到按量付费渠道。
- FR-40：系统必须提供有适用边界的会话路由保持能力。
- FR-41：系统必须支持独立于管理员凭据的项目访问令牌。
- FR-42：系统必须按项目访问令牌执行模型授权、预算和并发限制。
- FR-43：系统必须展示刷新任务状态和数据最近成功更新时间。
- FR-44：系统必须对主动健康探测设置次数与费用预算。
- FR-45：系统必须对额度、授权和健康事件提供去重提醒。
- FR-46：系统必须提供不包含正文的请求详情与选路原因。
- FR-47：系统必须将断流或缺失用量标记为未完整统计。

## 6. 技术架构

```text
apps/web
    │
    ▼
apps/server
    ├── api                 管理 API 与 OpenAI-compatible API
    ├── domain              自有领域模型与业务规则
    ├── connectors          Provider/Coding Plan 接入器
    ├── router              模型映射、选择、重试、熔断
    ├── quota               套餐与额度窗口
    ├── usage               Token、费用、对账
    ├── health              探测、拥挤度、置信度
    ├── credentials         SecretRef 与 Keychain
    ├── repository          SQLite/PostgreSQL 数据访问
    └── scheduler           周期刷新与健康任务
```

### 6.1 Connector 合约

下列接口表达能力范围，不是要求所有平台实现同一个大接口。落地时按执行清单 §3.1 拆分模型转发、套餐读取、OAuth 与健康探测接口；不支持的方法返回稳定的 `unsupported`，不能伪造空数据：

```go
type Connector interface {
    ValidateCredential(ctx context.Context, ref SecretRef) error
    DiscoverModels(ctx context.Context, account Account) ([]Model, error)
    FetchSubscription(ctx context.Context, account Account) (Subscription, error)
    FetchQuota(ctx context.Context, account Account) ([]QuotaWindow, error)
    RefreshCredential(ctx context.Context, account Account) error
    Forward(ctx context.Context, request RelayRequest) (RelayResponse, error)
    ProbeHealth(ctx context.Context, target Target) (HealthSample, error)
}
```

### 6.2 核心数据表

| 数据表 | 用途 | 敏感信息规则 |
|---|---|---|
| `providers` | Provider 类型和能力 | 不存密钥 |
| `accounts` | 平台账户、状态和 SecretRef | 只存引用与指纹 |
| `models` | 逻辑模型和上游模型 | 不敏感 |
| `account_models` | 账户可用模型和映射 | 不敏感 |
| `quota_snapshots` | 套餐额度窗口快照 | 不存原始凭据响应 |
| `usage_events` | 请求 Token、费用和延迟 | 不存正文 |
| `health_samples` | 健康采样 | 不存正文 |
| `routing_policies` | 优先级、权重和规则 | 不敏感 |
| `price_versions` | 模型价格与生效时间 | 保留来源 |
| `audit_events` | 管理操作和路由决策 | 强制脱敏 |

## 7. 第三方代码融合策略

| 来源 | 许可证 | 直接移植候选 | 不移植范围 |
|---|---|---|---|
| OneAPI | MIT | 渠道能力、令牌约束、模型映射、分发、中继适配器 | 充值、兑换码、邀请、多租户销售逻辑 |
| CLIProxyAPI | MIT | Codex/Claude/Gemini CLI OAuth、Token 刷新、协议适配 | 独立服务生命周期、原配置体系、原管理接口 |
| CPA-Manager-Plus | MIT | 额度窗口解析、账户巡检、用量聚合、部分图表交互 | 独立 Manager Server、CPA 专属存储和登录体系 |
| New API | AGPLv3 + 附加署名要求 | 公开行为、接口和产品设计参考 | 默认不复制源码 |

### 7.1 移植规则

1. 先为目标行为编写兼容测试或样例。
2. 在 `docs/source-ledger.md` 登记来源提交和文件。
3. 把代码移入 Midroute 模块，改用 Midroute 类型和错误语义。
4. 删除对原项目全局配置、数据库和单例状态的依赖。
5. 增加超时、取消、脱敏和边界测试。
6. 通过测试后才从运行链路移除对应旧服务。
7. 保留必要的版权头，并汇总到 `THIRD_PARTY_NOTICES.md`。

## 8. 实施阶段

M0–M7 表示功能归属，不再要求机械串行：先补基础与账户界面，再提前验证一个 Coding Plan 的额度，随后完成一个真实 Coding 工具的调用闭环。具体按执行清单 D0–D4 和 MR-001–MR-032 的依赖执行；M6 页面随对应后端任务交付。

### M0：冻结新方向与保护现有数据

**目标：** 明确自主项目边界，停止继续扩展三服务串联方案。

**交付：**

- 本 PRD 评审通过。
- 创建第三方来源台账和许可证清单。
- 记录当前数据库、配置和运行数据的位置。
- 为当前 `data/` 生成只读备份方案。
- 将旧方案文档标记为历史，不直接删除。

**退出条件：** 用户确认 PRD；未删除现有数据库或凭据；许可证边界清楚。

### M1：自主工程骨架

**目标：** 建立 Midroute 唯一前后端入口。

**交付：**

- `apps/server`、`apps/web` 和核心内部模块。
- 配置加载、结构化日志、错误码、数据库 migration。
- `/healthz`、`/readyz`。
- 本地模式安全边界和基础管理会话。
- 根目录构建、测试和启动脚本。

**退出条件：** 单服务可启动；空数据库可自动初始化；自动测试通过。

### M2：OneAPI 核心能力移植

**目标：** 获得基础 API Key Provider 和统一中继能力。

**交付：**

- Provider、Account、Model 和 Token 数据模型。
- OpenAI、Anthropic、Gemini、OpenAI-compatible Connector。
- 模型发现、模型映射、渠道测试。
- `/v1/models`、`/v1/chat/completions` 流式与非流式路径。

**退出条件：** 至少三个官方 API Provider 和一个兼容 Provider 可通过统一 API 调用。

### M3：Coding Plan 与 OAuth

**目标：** 接入订阅型 Coding Plan。

**交付：**

- OAuth/PKCE 流程和刷新状态机。
- Codex/ChatGPT Subscription Connector。
- Claude Code、Gemini CLI 等 Connector 的可行性验证和逐项接入。
- OAuth 凭据 SecretRef 化。

**退出条件：** 至少一个 Coding Plan 可安全登录、刷新 Token、发现模型并转发请求。

### M4：额度、Token 与费用

**目标：** 统一展示套餐和消耗。

**交付：**

- 标准 Quota Window 模型。
- 5 小时、一周、日和月窗口展示。
- 本地 usage event 聚合。
- 官方 Usage/Cost Adapter 和对账。
- 价格版本管理。

**退出条件：** 测试账户能展示来源、可信度、重置时间和历史趋势；不可用数据不伪造。

### M5：健康、拥挤度与智能路由

**目标：** 根据真实观测选择渠道。

**交付：**

- HealthSample、评分、置信度、滞回和熔断。
- 优先级、权重、余额和健康度路由。
- 安全重试与失败分类。
- 路由决策审计。

**退出条件：** 故障注入测试能稳定触发熔断、回退和恢复；小样本不会误切换。

### M6：自主管理界面

**目标：** 形成统一中文产品体验。

**交付：**

- 总览、账户、模型、套餐、用量、健康、路由和设置页面。
- 首次配置向导。
- 实时刷新与后台任务状态。
- 本地免重复输入管理员密钥体验。

**退出条件：** 桌面和移动浏览器完成关键流程视觉验收；无残留 OneAPI/CPA 品牌和入口。

### M7：迁移与发布

**目标：** 从当前实验工程安全迁移到自主版本。

**交付：**

- OneAPI 渠道和模型映射导入器。
- 数据备份、恢复和 migration 验证。
- macOS arm64 包和 Docker 包。
- Secret scan、依赖审计、许可证清单和发布检查。

**退出条件：** 新环境安装、旧数据 dry-run 导入、备份恢复和完整冒烟测试全部通过。

## 9. 验证策略

- 单元测试：Connector 映射、额度窗口、价格、路由、脱敏和迁移。
- 合约测试：使用本地 mock server 固定第三方请求和响应。
- 集成测试：SQLite、HTTP 流式转发、取消、超时、Token 刷新和后台任务。
- 故障测试：429、401、403、5xx、慢响应、断流、无模型和额度耗尽。
- 安全测试：OAuth state/PKCE、SSRF、日志泄密、路径穿越和远程免登录绕过。
- 浏览器测试：所有 UI 用户故事必须完成真实浏览器验证。
- 发布门禁：test、typecheck、lint、build、secret scan、license scan、migration smoke 全部通过。

## 10. 成功指标

- 用户只需启动一个服务即可使用管理台和统一 API。
- 新增普通 API Key Provider 的代码改动限定在独立 Connector 和注册表范围内。
- 支持的平台中，模型发现成功率达到 99%（排除上游故障与无权限账户）。
- 本地请求用量记录丢失率低于 0.1%。
- 健康探测不会把密钥、Prompt 或响应正文写入日志。
- 已支持的额度数据全部显示来源和可信度，不出现来源不明的精确数字。
- 服务重启后配置、额度历史和登录会话按设计恢复。
- 常规开发构建不再需要编译或启动三个上游项目。

## 11. 风险与控制

| 风险 | 影响 | 控制措施 |
|---|---|---|
| Coding Plan 没有稳定公开 API | 无法持续读取额度 | 能力探测、`unavailable` 状态、版本化 Connector、禁止网页抓取兜底 |
| 移植代码仍依赖原项目全局状态 | 自主工程演变为拼接工程 | 先定义 Connector 合约，再迁移逻辑；禁止直接导入原项目模块 |
| OAuth Token 泄露 | 账户安全风险 | Keychain、SecretRef、最小日志、自动脱敏和权限收紧 |
| 多次重试导致重复请求 | 费用和数据错误 | 失败分类、幂等判断、有限重试、客户端取消传播 |
| SQLite 并发写冲突 | 用量丢失或任务失败 | WAL、busy timeout、短事务、单写入队列和压力测试 |
| 上游协议频繁变化 | Connector 失效 | 合约测试、来源提交台账、定期上游差异评估 |
| 许可证污染 | 无法闭源或发布 | MIT 优先、AGPL 不复制、第三方 notice 和发布前 license scan |

## 12. 数据迁移与回退

- 重构期间不覆盖或删除当前 `data/`。
- 新自主服务使用新的数据库文件，旧数据库只读。
- 导入过程先生成 dry-run 报告，再由用户确认执行。
- 所有 migration 在备份后运行，并记录 schema 版本。
- M1–M6 期间可回退到当前实验服务，但两边不共享写入中的 SQLite。
- 等自主版本达到 M7 验收条件后，再单独评审删除旧服务、旧二进制和上游工作副本。

## 13. 待确认问题

1. 项目是否计划闭源商业发布？当前默认按“可能闭源”处理。
2. V0.1 是否只支持单个本地管理员？当前默认是。
3. 首批 Coding Plan 的优先级是否为 Codex、Claude Code、Kimi、GLM、豆包？
4. 是否接受 V0.1 先支持 macOS，Linux/Docker 后置？
5. 管理界面是完全重新设计，还是允许选择性移植 CPA 的 MIT UI 组件？

## 14. 当前执行入口

[实施步骤与验收清单](./midroute-implementation-checklist.md) 已将本 PRD 及新增需求拆成 MR-001–MR-032 本地任务，包含实现方法、目标文件、依赖、步骤、验收和编码规范。

已有来源台账、工程骨架和基础中继应复用；本轮代码核查发现的缺口按执行清单补齐。M0–M2 尚不能统一视为完整交付；不把测试通过等同于产品功能完成。

首个交付目标：添加一个真实账户 → 显示模型及可读取额度 → 配置一个常用 Coding 工具 → 完成请求 → 查看用量、选路原因和状态变化。真实账户能力未验证时，保留待验收状态，不承诺套餐自动识别已实现。
