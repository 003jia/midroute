# Midroute 数据合约 v1（data-contracts-v1）

> 版本：v1（2026-09-07，随 schema migration v2 引入）。变更规则：字段语义修改必须升版本并新建文档；枚举值只增不改名；未知语义一律 `null`，与真实 `0` 严格区分。
> 本文档是领域类型（`internal/domain`）、数据库结构（`internal/db`）与 API 输出的一致性基准。

## 1. 枚举

### accounts.mode（账户模式）
| 值 | 语义 |
|---|---|
| `monitor_only` | 仅监测：只读取套餐/额度/健康，**永不成为路由候选** |
| `relay_and_monitor` | 转发并监测（默认） |

### accounts.auth_type（认证类型）
`api_key` | `oauth` | `manual`

### accounts.billing_mode（计费方式）
`subscription` | `metered` | `unknown`（默认；不从 Key 外观猜测）

### accounts.auth_state（认证可用状态，与启停 status 分列）
`ok` | `expired` | `reauth_required` | `unknown`（默认）

### accounts.status（启停，沿用 v1）
`active` | `disabled` | `error`

### capability（能力三态）
`supported` | `unsupported` | `unknown`
平台声明某能力 ≠ 当前凭据权限足够；声明与实测分列，实测结果附原因与检查时间。

## 2. 空值与单位语义

1. 未知数字在数据库中用 `NULL`，在 JSON 中用 `null`；已知零必须存 `0`。禁止用 0、-1 或空串表示"未知"。
2. 计量字段显式带单位（`tokens/requests/percent/currency` + 币种）；百分比无已知分母时不得换算成 Token。
3. 时间一律 UTC（`RFC3339`），界面层再转用户时区。`collected_at`（采集时间）、`last_success_at`（最近成功）、`expires_at`（失效时间）三者语义不同，不得混用。
4. 失效刷新保留上次成功值并标记过期，不得清零。

## 3. 模型身份

- 模型内部 ID：`{provider_id}|{upstream_id}`；数据库另有 `UNIQUE(provider_id, upstream_id)` 约束兜底（migration v2）。
- 同名模型属于不同 Provider 时互不覆盖；路由候选必须用内部 ID 明确引用。
- 持久记录同时保留内部引用与当时上游标识；历史归属不依赖仍存活的账户行。

## 4. 数据来源与可信度（预留，MR-008/009 起使用）

| 维度 | 取值 |
|---|---|
| source | `official` / `reported` / `observed` / `estimated` / `manual` |
| confidence | `exact` / `reported` / `estimated` / `unavailable` |
| availability | `available` / `unsupported` / `permission_required` / `reauth_required` / `error` / `unknown` |
| freshness | `fresh` / `stale` / `unknown` |

三个维度不能合并成一个枚举。

## 5. 迁移与兼容

- migration 单向追加：已发布版本禁止修改；每个迁移在独立事务内执行，失败回滚并拒绝启动。
- 数据库 schema 版本高于程序已知版本时，程序拒绝启动并提示升级（`db.Migrate` 前向拒绝）。
- 当前版本：v2（`accounts` 增 mode/auth_type/billing_mode/auth_state；`models` 增唯一索引）。
