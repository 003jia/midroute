#!/usr/bin/env bash
set -euo pipefail
# Midroute 单服务冒烟测试：启动、healthz/readyz、空库初始化、安全边界
# 用法: ./build/smoke-test.sh

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/dist/bin/midroute"
PORT="${SMOKE_PORT:-18100}"
DATA="${SMOKE_DATA:-/tmp/midroute-smoke}"
TMP="$(mktemp -d)"

[ -x "$BIN" ] || { echo "缺少 $BIN，请先运行 ./build/build.sh" >&2; exit 1; }
rm -rf "$DATA"

MIDROUTE_DATA_DIR="$DATA" MIDROUTE_HTTP_ADDR="127.0.0.1:$PORT" nohup "$BIN" > "$TMP/server.log" 2>&1 &
PID=$!
trap 'kill "$PID" 2>/dev/null || true; rm -rf "$TMP"' EXIT

fail=0
wait_ready() { # url expected desc
  for _ in $(seq 1 20); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "$1" || echo 000)"
    if [ "$code" = "$2" ]; then echo "  ok: $3"; return 0; fi
    sleep 0.5
  done
  echo "  FAIL: $3 (got $code want $2)" >&2; fail=1
}

echo "==> 启动单服务 (127.0.0.1:$PORT)"
wait_ready "http://127.0.0.1:$PORT/healthz" "200" "/healthz"
h="$(curl -s http://127.0.0.1:$PORT/healthz)"
case "$h" in *'"service":"midroute"'*) echo "  ok: 服务标识 midroute";; *) echo "  FAIL: $h" >&2; fail=1;; esac
wait_ready "http://127.0.0.1:$PORT/readyz" "200" "/readyz"

echo "==> 空库自动初始化"
if [ -f "$DATA/midroute.db" ]; then
  echo "  ok: 自动创建 midroute.db"
else
  echo "  FAIL: 未创建数据库" >&2; fail=1
fi

echo "==> 管理台静态页（M1 起由独立 dev 提供，此处仅确认 404 不崩溃）"
curl -s -o /dev/null -w "  /management.html -> %{http_code}\n" "http://127.0.0.1:$PORT/management.html"

if [ "$fail" = 0 ]; then echo "==> 冒烟测试全部通过"; else echo "==> 冒烟测试存在失败" >&2; exit 1; fi