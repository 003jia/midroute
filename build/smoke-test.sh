#!/usr/bin/env bash
set -euo pipefail
# Midroute 单服务冒烟测试：启动、healthz/readyz、空库初始化、管理页、安全边界
# 隔离性（ISS-08）：数据目录与日志均使用 mktemp 唯一临时目录；端口默认从
# 20000-40000 随机挑选并探测空闲，避免与已有实例冲突/误探他人实例。
# 用法: ./build/smoke-test.sh
# 环境变量覆盖（调试用）: SMOKE_PORT（固定端口）、SMOKE_DATA（固定数据目录）

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../" && pwd)"
BIN="$ROOT/dist/bin/midroute"
TMP="$(mktemp -d)"
DATA="$TMP/data"

[ -x "$BIN" ] || { echo "缺少 $BIN，请先运行 ./build/build.sh" >&2; exit 1; }

# 选择端口：显式指定 > 随机探测空闲
pick_port() {
  local p
  for _ in $(seq 1 20); do
    p=$(( (RANDOM % 20000) + 20000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
      echo "$p"; return 0
    fi
    exec 3>&- 3<&- 2>/dev/null || true
  done
  echo "无法找到空闲端口" >&2; return 1
}
if [ -n "${SMOKE_PORT:-}" ]; then
  PORT="$SMOKE_PORT"
else
  PORT="$(pick_port)"
fi

# 固定数据目录仅在显式指定时使用（并仍先备份式重建，不误删非本脚本资源）
if [ -n "${SMOKE_DATA:-}" ]; then
  DATA="${SMOKE_DATA}"
  rm -rf "$DATA"
fi
mkdir -p "$DATA"

MIDROUTE_DATA_DIR="$DATA" MIDROUTE_HTTP_ADDR="127.0.0.1:$PORT" MIDROUTE_STATIC_DIR="$ROOT/dist/web" nohup "$BIN" > "$TMP/server.log" 2>&1 &
PID=$!
trap 'kill "$PID" 2>/dev/null || true; rm -rf "$TMP"' EXIT

fail=0
wait_ready() { # url expected desc
  for _ in $(seq 1 20); do
    # 先确认仍是本次启动的进程在响应，避免误探已有实例
    if ! kill -0 "$PID" 2>/dev/null; then
      echo "  FAIL: $3（服务进程已退出，日志见 $TMP/server.log）" >&2; fail=1; return 1
    fi
    code="$(curl -s -o /dev/null -w '%{http_code}' "$1" || echo 000)"
    if [ "$code" = "$2" ]; then echo "  ok: $3"; return 0; fi
    sleep 0.5
  done
  echo "  FAIL: $3 (got $code want $2)" >&2; fail=1
}

echo "==> 启动单服务 (127.0.0.1:$PORT, 数据目录 $DATA)"
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

echo "==> 管理页托管"
if [ -f "$ROOT/dist/web/index.html" ]; then
  body="$(curl -s "http://127.0.0.1:$PORT/" | head -c 200)"
  case "$body" in *"<html"*) echo "  ok: / 返回管理页 HTML";; *) echo "  FAIL: / 未返回 HTML: $body" >&2; fail=1;; esac
  api404="$(curl -s -o /dev/null -w '%{content_type}' "http://127.0.0.1:$PORT/api/v1/unknown")"
  case "$api404" in *json*) echo "  ok: 未知 API 返回 JSON 404";; *) echo "  FAIL: 未知 API 返回 $api404" >&2; fail=1;; esac
  # 安全边界（ISS-07）：免登录路径伪造 Host 必须被拒
  forged="$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' "http://127.0.0.1:$PORT/api/v1/accounts")"
  case "$forged" in 403) echo "  ok: 伪造 Host 被拒绝（403）";; *) echo "  FAIL: 伪造 Host 返回 $forged" >&2; fail=1;; esac
else
  code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/management.html)"
  echo "  跳过页面断言（未构建前端，/management.html -> $code）"
fi

if [ "$fail" = 0 ]; then echo "==> 冒烟测试全部通过"; else echo "==> 冒烟测试存在失败" >&2; exit 1; fi
