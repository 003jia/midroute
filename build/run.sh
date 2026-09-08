#!/usr/bin/env bash
set -euo pipefail
# Midroute 单服务本地运行（默认仅监听 127.0.0.1，本地免登录）
# 用法: ./build/run.sh

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/dist/bin/midroute"
DATA="${MIDROUTE_DATA_DIR:-$ROOT/data}"
PORT="${MIDROUTE_HTTP_ADDR:-127.0.0.1:18100}"

[ -x "$BIN" ] || { echo "缺少 $BIN，请先运行 ./build/build.sh" >&2; exit 1; }
mkdir -p "$DATA"

echo "======================================================"
echo " Midroute 中间路由平台（自主工程 v$(cat "$ROOT/VERSION" 2>/dev/null || echo dev)）"
echo "   管理台/API: http://$PORT"
echo "   数据目录:   $DATA"
echo "   模式:       本地（仅 loopback）"
echo "   远程访问需设置 MIDROUTE_ADMIN_KEY 并关闭本地限制"
echo "======================================================"

exec env MIDROUTE_DATA_DIR="$DATA" MIDROUTE_HTTP_ADDR="$PORT" MIDROUTE_STATIC_DIR="$ROOT/dist/web" "$BIN"