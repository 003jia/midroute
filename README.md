# Midroute · 自主中间路由平台

本地优先的 AI 接入、凭据、套餐额度、用量与路由管理平台。目标是**单服务**：一个后端、一个前端、一个数据库、一个 API 地址；当前后端主体、账户/模型/额度页面和多项协议底座已经实现，真实账号验收、完整路由监测和发布能力仍在开发中。

> 产品范围：[Midroute PRD](tasks/prd-midroute-independent-platform.md)。
> 开发入口：[任务总览与实施清单](tasks/midroute-implementation-checklist.md)（2026-09-18 整理，32 项任务，48 个已完成子项、69 个待办）。
> 最新核对：[状态与证据](outputs/validation/PROJECT-STATUS-2026-09-18.md)；当前缺陷：[问题清单](tasks/midroute-code-audit-issues.md)；继续开发：[任务提示词](tasks/midroute-remaining-work-prompt.txt)。
> 第三方代码来源与许可证：`docs/source-ledger.md`、`THIRD_PARTY_NOTICES.md`。
> 运行数据位置：`docs/data-inventory.md`。

## 核心原则

- **自主工程**：不依赖 OneAPI / CLIProxyAPI / CPA-Manager-Plus 运行；它们只作为 MIT 代码来源逐模块移植。
- **单一入口**：`apps/server`（后端）+ `apps/web`（前端）。
- **凭据安全**：只存 SecretRef（Keychain + 指纹），不落明文；日志/导出统一脱敏。
- **数据可信**：按执行清单区分来源（official/reported/observed/estimated/manual）、可信度、可用状态和新鲜度，未知数不以零代替。
- **AGPL 不复制**：New API 仅参考。

## 目录结构

```
apps/server         后端（Go，唯一入口 cmd/server）
  internal/config   配置（默认仅监听 loopback；远程必须设 AdminKey）
  internal/db       SQLite + 单向版本化 migration（空库自动初始化）
  internal/errs     稳定错误码与 HTTP 语义
  internal/session  本地安全边界（loopback 免登录 / 远程鉴权）
  internal/httpserver  /healthz /readyz 与中间件
  internal/credentials  Keychain/Vault + 脱敏（G3）
  internal/health   健康/拥挤度评分引擎（G1）
  internal/usage    官方账单对账适配器（G2）
  cmd/vaultctl|healthcheck|billingcheck  运维工具
apps/web            前端（React 19 + TS + Vite；账户、模型、额度页已实现）
build/              构建/测试/冒烟/备份/上游评估/密钥门禁
work/               上游参考代码（只读，不入库）
docs/               台账、许可证、数据清单
outputs/history/    旧方案文档（HISTORICAL，存档不删除）
tasks/              PRD、实施步骤与验收清单
```

## 快速开始

```bash
./build/build.sh        # 单服务构建 → dist/
./build/smoke-test.sh   # 冒烟（healthz/readyz/空库初始化）
./build/run.sh          # 本地运行（127.0.0.1:18100，本地免登录）
./build/test.sh         # go test + 前端 typecheck/lint + 密钥门禁
```

开发模式：`cd apps/server && go run ./cmd/server`，`cd apps/web && npm run dev`（vite 代理到 18100）。

## 里程碑状态（2026-09-18 核对）

- M0/M1 核心基础已有验证：构建、静态页托管、会话/CSRF/Host 防护、默认隔离冒烟、主库备份与扫描脱敏已实现；静态资源缺失处理、显式冒烟目录保护、嵌套扫描排除及恢复仍需补齐。
- M2 后端主体已实现：Provider/Account 生命周期、凭据轮换、能力矩阵、OpenAI Chat Completions、Responses、Anthropic Messages、项目令牌和请求尝试记录均已有代码与自动化测试。真实 Coding 工具兼容尚未验收。
- M3 部分实现：OAuth 管理 API（start/complete/flows/revoke）、`reauth_required`、SecretRef 旋转、Codex 被动额度解析均已接线；**真实账户授权和真实 5 小时/周窗口仍待凭据验证**。
- M4 部分实现：额度池、快照历史、手工补充、手动刷新执行器、价格版本和汇总 API 已实现；额度单位混用、缺价格/不完整用量及计费口径需修复，自动调度、预算、真实账单对账和用量页面未完成。
- M5 部分实现：健康评分引擎已有单测；TTFT 采样、主动探测、熔断、权重路由、提醒和实时健康界面未完成。
- M6 部分实现：账户、模型、额度页面已有实现；浏览器验收、项目令牌/请求详情、策略、健康、提醒、用量页面未完成。
- M7 部分实现：备份脚本、来源台账和构建门禁已具备；恢复、OneAPI 导入、macOS 安装包、Docker 和完整发布验收未完成。

当前代码快照为 `25123ff`。2026-09-18 实跑后端 `go test -race -count=1 ./...`、前端 type-check/lint/build，均通过；同代码基线于 9 月 16 日通过根目录 test/build/smoke。本轮只整理文档，未新增功能。

任务清单拆分后为 **48 个已完成子项、69 个待办，共 117 项**。旧口径 41/96 已移入历史记录；拆分同时补录已完成部分和撤回过度勾选，不据此计算产品完成百分比。真实账户、浏览器、Coding 工具和发布验收继续保留独立待办。

### 历史方案遗留资产对照

2026-09-04 方案已从「三个上游服务集成」切换为「自主单服务」，`outputs/history/` 中的 M0/M1/M2 交付说明均属旧架构，其 ✅ 结论**不计入**实施清单任何 MR 验收框。资产去向：

| 旧方案资产 | 现位置 / 现状 |
|---|---|
| 冻结上游 CLIProxyAPI / CPA-Manager-Plus / one-api | `work/` 只读克隆 + `PINNED_COMMITS`；仅作代码来源，不运行；接入选定模块前按 MR-006/030 逐个核验 |
| M1 构建脚本 | 已改造为单服务入口（`build.sh`/`test.sh`/`smoke-test.sh`）；`run-local.sh` 已删除，由 `run.sh` 取代；冒烟隔离性已按 MR-001 验收 |
| M2 G3 Keychain/Vault（旧 `internal/vault`） | `apps/server/internal/credentials` + `cmd/vaultctl`，已接线 |
| M2 G1 健康评分引擎（旧 `internal/health`） | 引擎已整合且有单测；采样尚未接入网关与路由（问题清单 ISS-14） |
| M2 G2 官方账单对账（旧 `internal/billing`） | `apps/server/internal/usage` + `cmd/billingcheck` + 用量汇总 API；真实账单对账和界面仍未完成 |
| M2 G6 中文产品化 | `README.md`、`docs/首次配置向导.md` 保留；账户、模型、额度页面已实现，其他管理页面仍待开发 |

开发时以 `tasks/midroute-implementation-checklist.md` 和 `tasks/midroute-code-audit-issues.md` 为准；历史文档只用于追溯决策依据（如冻结 SHA、安全审计结论）。

## 测试

```bash
cd apps/server && go test ./...        # 后端单测（config/db/errs/session/health/credentials/usage/httpserver）
cd apps/web && npm run type-check      # 前端类型检查
cd apps/web && npm run lint
./build/ci/secret-scan.sh              # 密钥门禁
./build/backup-data.sh                 # 备份默认 data/；默认排除 local.keys/data.key，恢复待实现
```
