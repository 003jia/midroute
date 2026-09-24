> HISTORICAL：2026-09-18 整理前的代码核查记录。保留旧问题描述和旧行号用于追溯，当前状态只看 `tasks/midroute-code-audit-issues.md`。

# Midroute 代码核查问题清单

> 记录：2026-09-07。来源：对照 [实施清单](../../tasks/midroute-implementation-checklist.md) 全部 MR 验收框做的全量代码核查（实跑 `./build/test.sh`、`./build/build.sh` + 四路并行代码审查）。
> 用途：每个问题标注关联任务与证据位置；对应验收框在问题修复并留存证据前保持 `[ ]`。修复后在本文件标记状态并回填清单。
>
> 当前状态快照：2026-09-14，HEAD `25123ff`。下表是当前状态；后续各问题的小节保留初始核查证据，不能把其中的旧文件行号视为当前实现。已完成的后续证据见 `outputs/validation/`。

## 问题总览

| 编号 | 严重度 | 关联任务 | 一句话描述 | 状态 |
|---|---|---|---|---|
| ISS-01 | P0 | MR-002 | 管理页 404 **已修复（2026-09-14）**：StaticDir 是死配置，httpserver 无任何静态托管 | **已修复（2026-09-08，a8ae51a：main.go 挂载 StaticDir + SPA 回落 + JSON 404 + 路径穿越测试）** |
| ISS-02 | P0 | MR-012 | router `streamed` 标志从未置 true，首块输出后防重放防护实际失效 | **已修复（2026-09-08，a8ae51a：onChunk 置位 + 相关测试）** |
| ISS-03 | P0 | MR-015 | 用量落库复用请求 context 且忽略错误：客户端取消即丢记录；流式中断被记为成功 | **已修复（2026-09-08，a8ae51a：终态写入改独立 context.WithTimeout(3s)）** |
| ISS-04 | P0 | MR-028 | 备份脚本不覆盖现行主库 `midroute.db`，且显式打包明文凭据 `local.keys` | **已修复（2026-09-14：midroute.db 在线备份、默认排除 local.keys/data.key（--include-secrets 显式携带）、manifest 含 app/schema 版本与 SHA-256；恢复入口仍属 MR-028 后续）** |
| ISS-05 | P0 | MR-001 | `.gitignore` 缺 `node_modules` 与 `apps/web/dist`，Git 尚无首次提交，`git add .` 会入库依赖 | **已修复（2026-09-08，dee9b78/a8ae51a：初始提交建立，secret-scan 通过；2026-09-14 补 `.mimosa/` 忽略）** |
| ISS-06 | P0 | MR-030 | secret-scan 命中时打印整行内容（含疑似密钥本身），不满足"只报告脱敏位置" | **已修复（2026-09-14：sed 截断为 文件:行号+提示， planted-key 自测通过）** |
| ISS-07 | P0 | MR-002 | 无会话机制、无 Host/Origin 校验、无 CSRF 防护：localOnly 下浏览器跨站可直写管理 API | **已修复（2026-09-14：免登录路径 loopback 字面量 Host 强制 + 写请求同源校验 + 会话 Cookie（HttpOnly/SameSite=Strict、滑动续期、退出失效））** |
| ISS-08 | P1 | MR-001 | 冒烟脚本固定端口 18100、固定数据路径 `/tmp/midroute-smoke` 且启动即 `rm -rf`，可误伤已有实例 | **已修复（2026-09-14：mktemp 唯一数据目录 + 随机空闲端口 + 进程存活校验 + trap 精确清理）** |
| ISS-09 | P1 | MR-012 | 上游错误分类靠字符串匹配（`safeToRetry`/`mapRelayError`），无结构化错误类型 | **已修复（2026-09-14：connectors 结构化 UpstreamError + ErrRateLimited/ErrAuth/ErrTimeout/ErrNetwork/ErrTruncated/ErrModelUnavailable/ErrUpstream；router/api/accounts 全部改用 errors.Is/As，新增 TestStructuredErrorClassification）** |
| ISS-10 | P1 | MR-012 | OpenAI 兼容连接器流式 usage 恒为空、流式出错丢弃真实错误改抛笼统错误 | 未修复 |
| ISS-11 | P1 | MR-015/019 | `reasoning_tokens` 列从不填充；流式路径 cache/reasoning 丢失；cache 读写价被合并 | 未修复 |
| ISS-12 | P1 | MR-003 | `models` 表无 `(provider_id, upstream_id)` DB 级唯一约束；未来 schema 版本不拒绝旧程序写入 | **已修复（2026-09-07，migration v2 + 前向拒绝 + 测试）** |
| ISS-13 | P1 | MR-015 | `audit_events` 表已建但全库零读写，无 `internal/audit` 包，管理操作无审计 | 未修复 |
| ISS-14 | P1 | MR-021 | 健康链路断链：`health_samples` 表零读写、网关不采 TTFT、无 health API，引擎仅 CLI 使用 | 未修复 |
| ISS-15 | P1 | MR-019 | `price_versions` 表有 schema 无任何读写代码；金额用 float64 累加；对账不校验币种/范围 | **部分修复（2026-09-14：定点 nano-USD、版本取价、usage summary API 已完成；真实账单对账与界面仍待完成）** |
| ISS-16 | P1 | MR-023 | 权重路由未实现：`Weight` 字段存在但从不参与选择，仓储写死 `weight=1, priority=0` | 未修复 |
| ISS-17 | P2 | MR-012/014 | Anthropic 流式翻译只处理 `text_delta`，工具调用块（`input_json_delta` 等）被静默丢弃 | **已修复（2026-09-14：/v1/messages 原生 SSE 透传，`input_json_delta`/tool 事件保留；真实 Claude Code 验收仍待完成）** |
| ISS-18 | P2 | MR-030 | `docs/source-ledger.md` 中 OneAPI 用短 SHA，与 `PINNED_COMMITS` 全 SHA 不一致 | **已修复（2026-09-07）** |
| ISS-19 | P2 | MR-003 | `SaveRoutingPolicy` 实际按 `id` upsert，与注释"upsert by alias"不符 | 未修复 |

## 详细说明与证据

### ISS-01 管理页 404 未修复（P0 · MR-002）

验收核心"单服务管理页"上一轮即失败，本轮确认仍存在：

- `apps/server/internal/httpserver/httpserver.go` 全文无 `http.Dir`/`http.FileServer`/`StripPrefix`/`index.html`，`New()` 只注册 `/healthz`、`/readyz`；页面路径全部落到 ServeMux 默认文本 404。
- `internal/config/config.go:25-26` 的 `StaticDir` 注释自认"M6 起使用"，环境变量/JSON 加载通道已通但全代码库无消费方；`cmd/server/main.go` 未读该字段。
- 无"生产缺静态资源启动报错"逻辑；无 SPA 回落；未知 API 返回文本 404 而非 JSON。
- `build/smoke-test.sh:42-43` 对 `/management.html` 只打印状态码，注释自认"仅确认 404 不崩溃"。

修复方向：httpserver 挂载 `http.FileServer`（`StaticDir` → `dist/web`），SPA 回落仅限管理页路径；启动时校验静态目录存在；补页面/路径穿越测试。

### ISS-02 流式防重放失效（P0 · MR-012）

- `internal/router/router.go:168` 声明 `streamed := false`，`:188-190` `if streamed { break }`，但 onChunk 回调内**没有任何置 true 的赋值**——首块已发给客户端后失败仍会切换下一候选重放，造成内容重复输出。
- 与 `internal/api/api.go:424` 的 `started` 标志不联动：api 层只保证自己不再写错误事件，挡不住 router 层换候选。

修复方向：onChunk 首次成功写出即置 `streamed = true`（并在 api 层同一状态源判定）；补"首块后断流不重放"测试。

### ISS-03 用量记录会丢、断流记成成功（P0 · MR-015）

- `internal/api/api.go:411` `_ = a.Store.RecordUsageEvent(r.Context(), ev)`：复用请求 context，客户端断开即 canceled，INSERT 失败且错误被 `_` 吞掉。
- 流式取消路径更糟：客户端断开 → `ForwardStream` 返回 ctx.Err() 且已输出 → 走 `api.go:442-451` 仍用已取消的 `r.Context()` 调 `recordUsage` → 必然写库失败。
- 流式已输出后中途失败也走 `api.go:451`，以空错误码、StatusCode=200 记为"成功"——正是验收标准禁止的"成功且 0 Token"。
- `usage_events` 无 `(request_id, attempt_id)` 唯一约束，`RecordUsageEvent` 裸 INSERT（`repository.go:263-269`），重复提交会重复计费；router 多次尝试不落库。

修复方向：终态写入用独立有界 context（`context.WithoutCancel` + 超时）；中断/缺 usage 落 `incomplete/unknown`；引入 attempt 维度与幂等键。

### ISS-04 备份范围与凭据边界错误（P0 · MR-028）

- `build/backup-data.sh:21-30` 只对旧 `usage.sqlite` 做在线备份；现行主库 `midroute.db`（`config.go:19,75` 默认路径）**完全不在备份范围**。
- `:30` 显式复制 `local.keys`（本地网关 Key 与 Admin Key，`docs/data-inventory.md` 标记高敏感），与验收"默认包无 local.keys 或明文凭据"直接冲突。
- `:24,26` sqlite3 失败/缺失时回退 `cp`，会复制运行中 WAL 数据库。
- manifest 只有文件名列表，无应用/schema 版本与校验和；全仓无恢复入口、无 migration smoke。

修复方向：用 SQLite online-backup API 备份 `midroute.db`；默认排除全部凭据文件；manifest 记版本+SHA；补独立恢复脚本与失败场景测试。

### ISS-05 .gitignore 不完整且仓库零提交（P0 · MR-001）

- `git log` 无任何提交，全部文件未跟踪；此时 `git check-ignore apps/web/node_modules` 与 `apps/web/dist` 均无匹配——首次提交若用 `git add .` 会把依赖与构建产物全部入库。
- 现有 `.gitignore` 只排除根 `/dist/`、本地配置、`/data/`、`/backups/`、`/work/` 等，缺 `node_modules/`、`apps/web/dist/`、缓存目录。

修复方向：先补 `.gitignore`，再按清单 MR-001 要求做"仅含审查过的源码、锁文件和文档"的初始提交（不得 `git add .`）。

### ISS-06 secret-scan 会回显疑似密钥（P0 · MR-030）

- `build/ci/secret-scan.sh:22` 用 `grep -HEn` 输出 `文件:行号:整行内容`，`:29-30` 将命中行原样写入 stderr——一旦命中，真实密钥会进入 CI 日志/终端记录，违背验收框"发现内容只报告脱敏位置"。

修复方向：命中只输出 `文件:行号`（`cut -d: -f1-2` 或 `grep -c` 计数 + 单独列位置），详情引导人工查看。

### ISS-07 管理面缺会话/Origin/CSRF 防护（P0 · MR-002）

- `internal/session/session.go:41-55`：loopback 且 localOnly 直接免登录放行；远程仅常量时间比对 Bearer。名为 session 包但无 cookie/登录态/过期。
- 无 Host/Origin 校验、无 CSRF token：localOnly 下浏览器内恶意页面对 `http://127.0.0.1:18100/api/v1/accounts/...` 发跨站 POST，RemoteAddr 仍是 127.0.0.1 即可通过；DNS rebinding 同理。管理 API 已含创建账户等写操作（`api.go:44`）。

修复方向：按 MR-002 步骤④实现受保护会话 + Host/Origin 校验 + 写请求 CSRF；不信任转发头。

### ISS-08 冒烟脚本可误伤已有实例（P1 · MR-001）

- `build/smoke-test.sh:8` 固定默认端口 18100：端口被占时新进程失败，但 `wait_ready`（:20-27）会探活到**已有实例**的 `/healthz` 而误判通过。
- `:9,13` 数据目录固定 `/tmp/midroute-smoke` 且启动无条件 `rm -rf`，可能删掉正在运行实例的数据；退出 trap 不清理该目录（残留）。

修复方向：唯一 `mktemp -d` 数据目录 + 动态端口；trap 精确清理本次创建的全部资源。

### ISS-09 错误分类靠字符串匹配（P1 · MR-012）

- `internal/connectors/openai.go:123-134` `classifyUpstreamError` 返回 `fmt.Errorf("upstream rate_limited")` 一类字符串错误；`router.go:199-214` `safeToRetry` 与 `api.go:457-471` `mapRelayError` 均用 `strings.Contains` 反向猜类型。
- 违反清单 §5.1"不得按 err.Error() 的英文片段判断限额或重试"。

修复方向：定义结构化可判定错误类型（`errors.Is/As` 分类），连接器返回带状态码/类别的错误值。

### ISS-10 OpenAI 流式 usage 与错误丢失（P1 · MR-012）

- `internal/connectors/openai.go:102-109` 流式路径 `usage := &Usage{}` 恒为空，从不解析流内 usage 块 → 流式请求落库全为 0 Token。
- `:107` 流式出错时丢弃真实错误，`return usage, classifyUpstreamError(0, nil)` 返回笼统错误。

修复方向：解析 SSE `usage`/final chunk；错误透传原始分类。

### ISS-11 Token 细分口径缺口（P1 · MR-015/019）

- `reasoning_tokens` 列存在但 `api.go:399-404` 从不填充（`CompletionTokensDetails.ReasoningTokens` 被忽略）；流式路径只填 prompt/completion，cache/reasoning 丢失。
- `internal/usage/anthropic.go:149` 把 cache 读/写合并为单一 `CacheTokens`，无法分别计价（`price_versions` 也只有单一 `cache_price`，`db.go:204`）。

### ISS-12 模型唯一性与 schema 前向兼容（P1 · MR-003）

- `models` 表仅 `id TEXT PRIMARY KEY`（`db.go:123-132`），`(provider_id, upstream_id)` 唯一键只靠应用层拼接 `"%s|%s"`（`repository.go:124-126`），无 DB 约束防脏数据。
- `db.go:47-90` `Migrate` 对数据库版本 > 程序已知版本不报错、不拒绝启动——旧程序可写新库。

### ISS-13 审计表建而未用（P1 · MR-015）

- `audit_events` DDL 存在（`db.go:211-219`），但 repository 无读写方法、全库无写入点、无 `internal/audit` 包。管理操作（建账户/建策略）目前无任何审计记录。

### ISS-14 健康采样链路断链（P1 · MR-021）

- 引擎（评分/置信度/滞回/unknown 语义）完整且有单测——已按引擎层勾选清单 MR-021 第 1 条；但：
  - `health_samples` 表（`db.go:175-185`）全库零读写；
  - 网关 `recordUsage` 只记 latency 到 `usage_events`，不采 TTFT，不喂健康引擎；
  - 无 `/api/v1/health` 路由；引擎仅被 CLI `cmd/healthcheck`（读 JSON 文件）使用；
  - `Classify(accountLimited)` 的额度信号无数据源。

### ISS-15 价格体系最初只有表没有代码（P1 · MR-019，部分修复）

2026-09-14 当前实现已提供 `price_versions` 定点 nano-USD 字段、按 `effective_at` 取价、按事件时价格版本聚合，以及 `GET /api/v1/usage/summary`；证据见 `outputs/validation/MR-019-2026-09-14.md`。本小节下方保留的是初始差距；真实上游账单对账、预算和界面仍是后续范围。

- `price_versions` 表（`db.go:198-209`）无任何 Go 读写：无按 `effective_at` 区间取价、无 token×价格 计算；`Cost.Amount` 为 `float64`（`usage/billing.go:88-96`），违反清单 §5.2"金额禁止 float64 累加"。
- `reconcile.go` 只对 Usage token 对账，不校验币种/统计范围；聚合 API（`/api/v1/usage/summary` 等）与仓储聚合查询均不存在。

### ISS-16 权重路由未实现（P1 · MR-023）

- `domain.Candidate.Weight` 字段（`domain.go:60`）从不参与选择；`router.go:76` 仅按 Priority 升序尝试。`repository.go:207` 写死 `VALUES(?,?,?,?,0,1,?,?)`（priority=0, weight=1）。
- 另：`SaveRoutingPolicy` 实际按 `id` upsert（`repository.go:195,208`），与注释宣称的"upsert by alias"不符（见 ISS-19）。

### ISS-17 Anthropic 流式丢工具块（P2 · MR-012/014，已修复）

2026-09-14 已改为原生 Anthropic SSE 透传，测试覆盖 `input_json_delta` 与工具事件保留；证据见 `outputs/validation/MR-014-2026-09-14.md`。真实 Claude Code 工具往返仍需单独验收。

- `internal/connectors/anthropic.go:301-314` 流式翻译只处理 `text_delta`；`content_block_start`/`input_json_delta`/tool_use 块被静默忽略——工具调用请求经流式路径会丢内容。

### ISS-18 台账 SHA 不一致（P2 · MR-030）

- `docs/source-ledger.md:14,57` OneAPI 用短 SHA `8df4a26`，`PINNED_COMMITS:18` 为全 SHA `8df4a2670b98266bd287c698243fff327d9748cf`。台账应记录完整 SHA。

### ISS-19 注释与实现不符（P2 · MR-003）

- `repository.go` `SaveRoutingPolicy` 注释称 upsert by alias，实现按 `id` 判断冲突；行为以代码为准，注释需修正，否则会误导后续按别名覆盖的实现者。

## 当前修复优先级建议

1. **ISS-10 + ISS-11**：补齐 OpenAI 流式 usage/错误及 reasoning、cache 细分，否则用量和费用不可信。
2. **ISS-13**：接入脱敏审计读写，覆盖账户、令牌、OAuth、策略和导入操作。
3. **ISS-14 + ISS-16**：把真实健康样本、TTFT、熔断和权重选择接入路由，形成“拥挤度 → 选路”的产品闭环。
4. **MR-008、MR-018**：使用真实凭据完成套餐读取和首个 Coding 工具闭环；缺凭据时保持待验收，不伪造结果。
5. **MR-028–032**：恢复、导入、来源/许可证门禁、安装包和完整发布验收。
