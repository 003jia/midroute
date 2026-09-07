#!/usr/bin/env bash
set -euo pipefail
# Midroute 测试：后端 go test + 前端 type-check/lint
# 用法: ./build/test.sh

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> apps/server go test"
(
  cd "$ROOT/apps/server"
  go test ./...
)

echo "==> apps/web type-check"
(
  cd "$ROOT/apps/web"
  [ -d node_modules ] || npm install --no-audit --no-fund
  npm run type-check
)

echo "==> apps/web lint"
(
  cd "$ROOT/apps/web"
  npm run lint
)

echo "==> secret-scan 门禁"
"$ROOT/build/ci/secret-scan.sh"

echo "==> 全部测试通过"