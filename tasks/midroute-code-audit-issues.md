# Midroute 当前问题与修复清单

> 更新：2026-09-18；代码：`25123ff`。仅记录当前实现和剩余缺口，任务勾选以 [实施清单](./midroute-implementation-checklist.md) 为准。
> 整理前的完整问题描述、旧行号和修复方向已保留在 [历史问题记录](../outputs/history/code-audit-before-2026-09-18.md)。
> “已修复”只覆盖表内明确范围；新增或重新发现的缺口保留未完成。代码位置均相对仓库根目录。

## 当前状态

| 编号 | 优先级 | 任务 | 已完成 / 剩余 | 状态 |
|---|---|---|---|---|
| ISS-01 | P0 | MR-002 | 管理页托管和 API JSON 响应已修复；生产缺静态目录仅警告，缺失资源仍 SPA 回落 | 部分修复 |
| ISS-02 | P0 | MR-012 | 首块发送后 streamed 置位，禁止整请求重放；router 测试覆盖 | 已修复 |
| ISS-03 | P0 | MR-015 | 请求终态使用独立有界 context；尝试幂等已实现；缺 usage 语义另见 ISS-10/11 | 已修复（取消与幂等范围） |
| ISS-04 | P0 | MR-028 | 默认主库在线备份、排除指定凭据文件、manifest 已实现；实际配置目录/打包白名单和恢复仍缺 | 部分修复 |
| ISS-05 | P0 | MR-001 | 已有源码提交；嵌套依赖和构建物已由 Git 忽略 | 已修复 |
| ISS-06 | P0 | MR-030 | 扫描命中仅输出文件/行号；嵌套依赖与产物排除仍缺 | 部分修复 |
| ISS-07 | P0 | MR-002 | 本地 Host、同源写校验、会话签发/注销已有测试 | 已修复（当前本地访问范围） |
| ISS-08 | P1 | MR-001 | 默认 mktemp/随机端口/进程检查已修复；显式目录覆盖风险单列 ISS-22 | 已修复（默认模式） |
| ISS-09 | P1 | MR-012 | 核心连接器/路由错误分类已改 errors.Is/As；OAuth 重授权仍有错误文案匹配 | 部分修复 |
| ISS-10 | P1 | MR-012/015 | OpenAI Chat 流式仍不提取 usage；真实错误已传给 Classify，旧“原始错误被丢弃”描述不再适用 | 部分修复 |
| ISS-11 | P1 | MR-015/019 | 请求尝试及价格已有 cache read/write/reasoning 字段；协议采集、未知值和计价包含关系未完整验证 | 部分修复 |
| ISS-12 | P1 | MR-003 | 模型唯一约束、未来 schema 拒绝、迁移事务回滚已有测试 | 已修复 |
| ISS-13 | P1 | MR-015 | 管理操作审计写入已存在；缺查询/完整覆盖，写入错误多处被忽略 | 部分修复 |
| ISS-14 | P1 | MR-021/022 | 健康评分引擎有单测；health_samples、TTFT、health API 和网关接线缺失 | 未修复 |
| ISS-15 | P1 | MR-019 | 定点价格/版本取价/汇总已实现；缓存计费包含关系、币种和真实对账待完成 | 部分修复 |
| ISS-16 | P1 | MR-023 | 优先级和基本过滤已有；Weight 未参与选择，缺额度/健康策略 | 未修复 |
| ISS-17 | P2 | MR-012/014 | /v1/messages 原生 SSE 路径保留工具事件；旧跨协议翻译路径不因此自动视为兼容 | 部分修复（原生路径已通过） |
| ISS-18 | P2 | MR-030 | 来源台账 OneAPI 完整 SHA 已对齐 | 已修复 |
| ISS-19 | P2 | MR-003/023 | SaveRoutingPolicy 注释称按 alias，实际按 id upsert | 未修复 |
| ISS-20 | P1 | MR-008/009 | percent 快照的 Limit 被写入窗口分钟；相对重置时间尚未完整进入快照 | 未修复 |
| ISS-21 | P1 | MR-019 | 缺价格聚合产生 NULL 却扫描 int64；cost_estimable 只有文案没有输出字段 | 未修复 |
| ISS-22 | P1 | MR-001 | 显式 SMOKE_DATA 直接进入 rm -rf，未限定本次临时目录 | 未修复 |
| ISS-23 | P1 | MR-016 | 并发按 tokenID 计数，同项目多令牌不共享总上限 | 未修复 |

## 关键剩余项及验收依据

### 数据可信度：ISS-10/11/15/20/21

- `apps/server/internal/connectors/openai.go`：ForwardStream 初始化空 Usage 后只转发 chunk/判断 DONE，没有解析 usage。补真实格式 fixture，未知用量不得变成已知零。
- `apps/server/internal/quota/service.go`：ObservationToSnapshots 使用 unit=percent，却把 WindowMinutes 写入 Limit；ResetAfterSeconds 没有转换/保留到快照。应分别保存额度和窗口时长，按真实观测时间处理有依据的相对重置值。
- `apps/server/internal/repository/repository.go`：AggregateUsage 的价格子查询可能返回 NULL，SUM 后扫描到 int64；混合已定价/未定价记录还可能只统计部分费用。需返回可解释的不可估算/部分估算状态。
- `apps/server/internal/api/api.go` 与 `domain/domain.go`：汇总 disclaimer 引用 cost_estimable，但实际响应结构没有这个标志。不得以这段文案证明缺价格场景已验收。
- 费用计算需验证 input 是否含 cached input、cache read/write 的独立价格、reasoning 是否为 output 子集，以及币种限制。价格版本测试仅证明版本取价，不证明全部计费口径。
- 上述新增判断来自源码核对，未另行执行缺价格/单位混用专项复现；现有测试通过不覆盖这些边界。

### 审计与调度：ISS-09/13，MR-010/015

- `repository.RecordAuditEvent` 已存在，账户、令牌、OAuth、项目、价格、额度等操作已有写入；旧“全库零读写”结论撤回。
- 多处调用使用 `_ = RecordAuditEvent(...)`；未找到审计查询 API。补覆盖范围、查询、失败处理和脱敏回归。
- `api/responses_oauth.go` 仍按“需要重新授权”错误文案判断状态，应完成结构化错误收口。
- `scheduler.Enqueue` 同步执行；next_run_at 只是记录，当前无自动消费循环。原“429、授权失效、休眠恢复均已验收”撤回。
- `scheduler.runJob` 忽略终态写入错误；自动重试、限流退避、授权暂停和恢复需要独立测试。

### 生命周期与交付：ISS-01/04/06/22/23

- `cmd/server/main.go`：静态目录缺失仅日志警告，staticHandler 对缺失文件回落 index.html；补生产模式检查及资源 404 测试。
- `api/api.go`：删除账户缺少凭据清理失败的持久重试；MR-004 核心 CRUD 已完成，整项仍保留缺口。
- `build/backup-data.sh`：默认主库备份功能已有历史实跑；源固定为根 data/，仍复制旧配置/目录，需核验实际配置与白名单，恢复另行验收。
- `build/ci/secret-scan.sh`：已脱敏，但只按根路径排除 node_modules/dist；嵌套 apps/web 目录不在对应排除范围。
- `build/smoke-test.sh`：默认运行安全隔离；显式 SMOKE_DATA 分支直接删除传入目录。修复前不要使用该覆盖参数。
- `internal/access/access.go`：inUse 键为 tokenID。单令牌竞争测试已通过，项目多令牌总上限尚未实现。

## 开发顺序

1. MR-008/009/012/015/019：额度单位、流式计量、缺价格与费用完整度。
2. MR-010/017：自动调度、客户端配置及请求详情；同步完成已有页面浏览器验收。
3. MR-006/007/008/013/018：首个真实账户和 Coding 工具闭环。
4. MR-020–027：预算、健康、熔断、权重、提醒和完整页面。
5. MR-028–032：恢复、导入、来源门禁、安装与发布；ISS-22 路径保护可独立先修。

最新检查记录：[PROJECT-STATUS-2026-09-18](../outputs/validation/PROJECT-STATUS-2026-09-18.md)。
