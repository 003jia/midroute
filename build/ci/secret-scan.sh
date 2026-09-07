#!/usr/bin/env bash
set -euo pipefail
# 密钥扫描门禁：扫描仓库文件（排除 work/ 上游克隆与 .git），
# 出现疑似密钥模式即失败。用于 CI 与提交前检查。
# 用法: ./build/ci/secret-scan.sh [--allow-work]

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scan() {
  local file
  find "$ROOT" -type f \
    -not -path "$ROOT/.git/*" \
    -not -path "$ROOT/work/*" \
    -not -path "$ROOT/dist/*" \
    -not -path "$ROOT/data/*" \
    -not -path "$ROOT/backups/*" \
    -not -path "$ROOT/node_modules/*" \
    -not -name "*.lock" \
    -not -name "package-lock.json" \
    -not -name "*_test.go" \
    -print0 | while IFS= read -r -d '' file; do
      # 典型密钥/凭据模式（vaultctl 的假名、真实 Key、token）
      grep -HEnE '(sk-[A-Za-z0-9]{16,}|sk-ant-[A-Za-z0-9]{16,}|AIza[A-Za-z0-9_-]{16,}|Bearer [A-Za-z0-9._-]{20,}|api[_-]?key[[:space:]]*[:=][[:space:]]*["'"'"'][A-Za-z0-9]{16,}|cmp_admin_[A-Za-z0-9]{8,}|secret-key[[:space:]]*:[[:space:]]*[A-Za-z0-9]{8,})' "$file" 2>/dev/null \
        | grep -vE ':([0-9]+):[[:space:]]*(#|//|--|/\*|\*)' \
        || true
    done
}
hits="$(scan)"
if [ -n "$hits" ]; then
  echo "!! 检测到疑似密钥模式：" >&2
  echo "$hits" >&2
  exit 1
fi
echo "==> secret-scan: 未发现疑似密钥模式"
