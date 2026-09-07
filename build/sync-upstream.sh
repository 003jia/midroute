#!/usr/bin/env bash
set -euo pipefail
# 上游升级评估：fetch 最新，展示与冻结提交的差异摘要，不自动合并。
# 用法: ./build/sync-upstream.sh [apply]
#   默认: 只展示差异。传 apply: 更新 PINNED_COMMITS 并 checkout 到最新。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/PINNED_COMMITS"
MODE="${1:-dry-run}"

evaluate() {
  local dir="$1" repo="$2" pinned="$3" name="$4"
  echo "===== $name ($repo) ====="
  git -C "$dir" fetch origin 2>&1 | sed 's/^/  /' || true
  local latest
  latest="$(git -C "$dir" rev-parse origin/main 2>/dev/null || git -C "$dir" rev-parse origin/HEAD 2>/dev/null || echo "")"
  if [ -z "$latest" ]; then echo "  !! 无法解析远端最新提交"; return; fi
  echo "  冻结: $pinned"
  echo "  最新: $latest"
  if [ "$latest" = "$pinned" ]; then
    echo "  状态: 无更新"
  else
    local ahead behind
    behind="$(git -C "$dir" rev-list --count "$pinned..$latest" 2>/dev/null || echo "?")"
    ahead="$(git -C "$dir" rev-list --count "$latest..$pinned" 2>/dev/null || echo "?")"
    echo "  状态: 上游领先 $behind 提交（冻结端领先 $ahead 提交）"
    echo "  -- 提交摘要（最近 20 条）--"
    git -C "$dir" log --oneline --no-merges "$pinned..$latest" -20 2>/dev/null | sed 's/^/    /' || true
    echo "  -- 涉及文件（去重前 30）--"
    git -C "$dir" diff --name-only "$pinned" "$latest" 2>/dev/null | sed 's/^/    /' | head -30 || true
  fi
}

evaluate "$ROOT/work/CLIProxyAPI" "$GATEWAY_REPO" "$GATEWAY_COMMIT" "$GATEWAY"
evaluate "$ROOT/work/CPA-Manager-Plus" "$MANAGER_REPO" "$MANAGER_COMMIT" "$MANAGER"
evaluate "$ROOT/work/one-api" "$ONEAPI_REPO" "$ONEAPI_COMMIT" "$ONEAPI"

if [ "$MODE" = "apply" ]; then
  echo ""
  echo "==> apply 模式：更新冻结版本到最新"
  set_gateway="$(git -C "$ROOT/work/CLIProxyAPI" rev-parse origin/main 2>/dev/null || git -C "$ROOT/work/CLIProxyAPI" rev-parse origin/HEAD)"
  set_manager="$(git -C "$ROOT/work/CPA-Manager-Plus" rev-parse origin/main 2>/dev/null || git -C "$ROOT/work/CPA-Manager-Plus" rev-parse origin/HEAD)"
  set_oneapi="$(git -C "$ROOT/work/one-api" rev-parse origin/main 2>/dev/null || git -C "$ROOT/work/one-api" rev-parse origin/HEAD)"
  git -C "$ROOT/work/CLIProxyAPI" checkout "$set_gateway"
  git -C "$ROOT/work/CPA-Manager-Plus" checkout "$set_manager"
  git -C "$ROOT/work/one-api" checkout "$set_oneapi"
  # 更新 PINNED_COMMITS（保留手工记录的时间与说明占位）
  python3 - "$set_gateway" "$set_manager" "$set_oneapi" <<'PYEOF'
import re, sys
gw, mgr, oneapi = sys.argv[1], sys.argv[2], sys.argv[3]
p = "PINNED_COMMITS"
s = open(p).read()
s = re.sub(r'GATEWAY_COMMIT=.*', f'GATEWAY_COMMIT={gw}', s)
s = re.sub(r'MANAGER_COMMIT=.*', f'MANAGER_COMMIT={mgr}', s)
s = re.sub(r'ONEAPI_COMMIT=.*', f'ONEAPI_COMMIT={oneapi}', s)
s = re.sub(r'GATEWAY_DATE=.*', 'GATEWAY_DATE=' + __import__('datetime').datetime.now().astimezone().isoformat(), s)
s = re.sub(r'MANAGER_DATE=.*', 'MANAGER_DATE=' + __import__('datetime').datetime.now().astimezone().isoformat(), s)
s = re.sub(r'ONEAPI_DATE=.*', 'ONEAPI_DATE=' + __import__('datetime').datetime.now().astimezone().isoformat(), s)
s = re.sub(r'GATEWAY_SUBJECT=".*"', 'GATEWAY_SUBJECT="(见 sync-upstream apply 记录)"', s)
s = re.sub(r'MANAGER_SUBJECT=".*"', 'MANAGER_SUBJECT="(见 sync-upstream apply 记录)"', s)
s = re.sub(r'ONEAPI_SUBJECT=".*"', 'ONEAPI_SUBJECT="(见 sync-upstream apply 记录)"', s)
open(p, "w").write(s)
PYEOF
  echo "==> 已更新并 checkout。请重新运行 build.sh + smoke-test.sh 验证。"
  echo "    !!! apply 前请先人工评审上面的差异摘要。"
else
  echo ""
  echo "==> dry-run：未做任何变更。确认差异后使用: ./build/sync-upstream.sh apply"
fi
