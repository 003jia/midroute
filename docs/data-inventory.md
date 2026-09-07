# 运行数据与配置位置清单（Data Inventory）

> 依据 PRD M0：「记录当前数据库、配置和运行数据的位置」。
> 2026-09-04 记录。用于备份、迁移与回退规划。

## 1. 运行数据（`data/`）

| 路径 | 内容 | 敏感 |
|---|---|---|
| `data/usage.sqlite`（含 -wal/-shm） | CPA-Manager-Plus 用量库（账户/用量/额度历史） | 中（不含明文凭据） |
| `data/data.key` | CPA 用量库加密密钥 | 高（备份需同时保留） |
| `data/local.keys` | 本地生成的网关 Key 与 Admin Key | 高 |
| `data/gateway.local.yaml` | 网关本地配置 | 中 |
| `data/usage-imports/` | 用量导入会话 | 中 |
| `data/oneapi/`（含 `one-api.db`） | OneAPI 运行数据 | 高（可能含令牌相关数据） |
| `data/static/management.html` | 上游管理台静态页 | 低 |
| `data/*.log` | 运行日志 | 需脱敏 |

## 2. 上游参考代码（`work/`，只读参考）

| 路径 | 仓库 | 提交 |
|---|---|---|
| `work/CLIProxyAPI` | router-for-me/CLIProxyAPI | `17a65ee` |
| `work/CPA-Manager-Plus` | seakee/CPA-Manager-Plus | `be3039b` |
| `work/one-api` | songquanpeng/one-api | `8df4a26`（含 2 处未提交实验改动） |

## 3. 用户级凭据（OS Keychain / 用户目录）

| 路径 | 内容 | 敏感 |
|---|---|---|
| `~/.cli-proxy-api/` | 上游 OAuth/API Key 登录凭据（明文 JSON，实验期遗留） | 高 |
| macOS Keychain `midroute` 服务 | 自有 vaultctl 写入的凭据 | 高 |

## 4. 构建产物（`dist/`）

旧双上游构建产物，含三个上游二进制；M7 完成后按评审删除。

## 5. 迁移注意事项

- M1–M6 期间旧库与新库并存，不共享写入中的 SQLite。
- 自主服务使用**新的**数据库文件；`data/` 只读。
- 备份必须包含 `data/` 全量 + `data.key`；不含明文凭据的数据库同样需要 Keychain 导出方案。