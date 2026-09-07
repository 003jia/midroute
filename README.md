# Midroute · 自主中间路由平台

本地优先的 AI 接入、凭据、套餐额度、用量与路由管理平台。目标是**单服务**：一个后端、一个前端、一个数据库、一个 API 地址；当前后端基础已实现，管理前端仍为骨架。

> 产品范围：[Midroute PRD](tasks/prd-midroute-independent-platform.md)。
> 开发入口：[实施步骤与验收清单](tasks/midroute-implementation-checklist.md)（2026-09-07，32 项任务，含实现方法、依赖、步骤及编码规范）。
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
apps/web            前端（React 19 + TS + Vite 骨架）
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

## 里程碑状态（2026-09-07 核查）

- M0 基本完成：来源台账、数据清单、历史文档和旧数据备份已存在；备份含敏感文件，尚不满足安全恢复交付要求。
- M1 部分完成：后端启动、SQLite migration、健康接口和根目录脚本已存在；静态页面托管及完整访问边界仍需验收。
- M2 后端主体已实现：API Key Connector、模型发现、基础选路、流式/非流式中继及用量记录已有代码与 mock 测试；账户完整管理、可靠重试及真实客户端验收未完成。
- M3 部分实现（2026-09-07）：Codex OAuth 授权码+PKCE/单飞刷新（`internal/connectors/oauth`，mock 层验证）、Codex 额度窗口被动解析（`internal/quota`）、平台能力矩阵（`docs/provider-capabilities.md`）已就绪；**真实账户授权与真实窗口数值验收待真实凭据**，OAuth 管理 API 接线待 MR-002 会话安全。
- M4–M5 部分底层代码：已有账单适配器与健康评分，未形成额度采集、统计、监测及智能路由闭环。
- M6 前端骨架；M7 部分脚本准备。完整页面、导入恢复和发布验收待完成。

检查通过记录来自本对话前一轮：Go 测试、前端 type-check/lint、密钥扫描及基础冒烟通过；该冒烟允许 `/management.html` 返回 404，不能作为管理界面交付证据。本次更新为计划文档，不代表新增功能已实现。

### 历史方案遗留资产对照

2026-09-04 方案已从「三个上游服务集成」切换为「自主单服务」，`outputs/history/` 中的 M0/M1/M2 交付说明均属旧架构，其 ✅ 结论**不计入**实施清单任何 MR 验收框。资产去向：

| 旧方案资产 | 现位置 / 现状 |
|---|---|
| 冻结上游 CLIProxyAPI / CPA-Manager-Plus / one-api | `work/` 只读克隆 + `PINNED_COMMITS`；仅作代码来源，不运行；接入选定模块前按 MR-006/030 逐个核验 |
| M1 构建脚本 | 保留并改造为单服务入口（`build.sh`/`test.sh`/`smoke-test.sh`）；`run-local.sh` 已删除，由 `run.sh` 取代；冒烟隔离性需按 MR-001 重做 |
| M2 G3 Keychain/Vault（旧 `internal/vault`） | `apps/server/internal/credentials` + `cmd/vaultctl`，已接线 |
| M2 G1 健康评分引擎（旧 `internal/health`） | 引擎已整合且有单测；采样尚未接入网关与路由（问题清单 ISS-14） |
| M2 G2 官方账单对账（旧 `internal/billing`） | `apps/server/internal/usage` + `cmd/billingcheck`；未形成 API 闭环（ISS-15） |
| M2 G6 中文产品化 | `README.md`、`docs/首次配置向导.md` 保留；前端页面仍为骨架 |

开发时以 `tasks/midroute-implementation-checklist.md` 和 `tasks/midroute-code-audit-issues.md` 为准；历史文档只用于追溯决策依据（如冻结 SHA、安全审计结论）。

## 测试

```bash
cd apps/server && go test ./...        # 后端单测（config/db/errs/session/health/credentials/usage/httpserver）
cd apps/web && npm run type-check      # 前端类型检查
cd apps/web && npm run lint
./build/ci/secret-scan.sh              # 密钥门禁
./build/backup-data.sh                 # 只读备份（含 data.key）
```
