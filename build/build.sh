#!/usr/bin/env bash
set -euo pipefail
# Midroute 单一服务构建：后端 + 前端 → dist/
# 用法: ./build/build.sh [--skip-web]

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
VERSION="$(cat "$ROOT/VERSION" 2>/dev/null || echo 0.1.0-dev)"
mkdir -p "$DIST/bin" "$DIST/web"

echo "==> Midroute v$VERSION 构建"

# 1. 后端（唯一后端入口 cmd/server）
echo "==> [1/3] 构建 apps/server"
(
  cd "$ROOT/apps/server"
  go build -o "$DIST/bin/midroute" ./cmd/server
)

# 2. 前端
if [ "${1:-}" != "--skip-web" ]; then
  echo "==> [2/3] 构建 apps/web"
  (
    cd "$ROOT/apps/web"
    [ -d node_modules ] || npm install --no-audit --no-fund
    npm run build
  )
  cp -R "$ROOT/apps/web/dist/." "$DIST/web/"
fi

# 3. 自有运维工具（开发/运维用，不参与服务运行；cmd/server 已作为主二进制 midroute 构建）
echo "==> [3/3] 构建运维工具"
(
  cd "$ROOT/apps/server"
  for tool in vaultctl healthcheck billingcheck; do
    go build -o "$DIST/bin/$tool" "./cmd/$tool"
  done
)

# 4. 产物清单
cat > "$DIST/MANIFEST.txt" <<EOF
midroute v$VERSION (自主工程)
built_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)
binaries: midroute (server), vaultctl, healthcheck, billingcheck
frontend: dist/web (React)
EOF

echo ""
echo "==> 构建完成"
ls -lh "$DIST/bin"