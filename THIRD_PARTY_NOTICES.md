# 第三方许可证清单（THIRD_PARTY_NOTICES）

> 本文件汇总 Midroute 可能融合的第三方代码的许可证与版权声明。
> 实际融合的每一段代码，其原始版权与许可声明必须随代码保留，并在此登记。

## 1. OneAPI — MIT License

- 仓库：https://github.com/songquanpeng/one-api
- 提交：`8df4a26`
- 版权：Copyright (c) 2023 songquanpeng
- 说明：主要移植候选（渠道、令牌、模型映射、中继适配器）。

MIT License 原文摘要（完整文本见仓库 LICENSE）：

```
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
```

## 2. CLIProxyAPI — MIT License

- 仓库：https://github.com/router-for-me/CLIProxyAPI
- 提交：`17a65ee5470fbaf0e22fc219381e6a4ae9e07624`
- 版权：见仓库 LICENSE（MIT）
- 说明：OAuth 与协议适配代码来源。

## 3. CPA-Manager-Plus — MIT License

- 仓库：https://github.com/seakee/CPA-Manager-Plus
- 提交：`be3039b66917a13e91b51660ecc10c08dca4862a`
- 版权：见仓库 LICENSE（MIT）
- 说明：额度窗口、用量聚合与观测界面来源。

## 4. New API — AGPLv3 + 附加署名要求（仅参考，不复制）

- 仓库：https://github.com/Calcium-Ion/new-api
- 许可证：GNU Affero General Public License v3.0（含附加署名要求）
- 规则：默认不复制其源码；仅参考公开行为、接口与产品设计。

## 5. Midroute 自有代码

- Midroute 自身代码（本仓库根，`apps/` 等自有实现）默认按可闭源边界设计，许可证待产品决策确认后补充。

## 发布门禁提醒

- 发布前执行许可证扫描（PRD §9、§11），确保仅含允许融合的代码。
- 任何新增第三方依赖/移植，先在本文件与 `docs/source-ledger.md` 登记。