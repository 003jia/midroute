#!/usr/bin/env bash
set -euo pipefail

# 将本地 CLIProxyAPI 注册为 OneAPI 的 OpenAI-compatible 渠道。
# 该脚本只操作本机 loopback 服务，不打印任何访问令牌或渠道密钥。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA="${MIDROUTE_DATA_DIR:-$ROOT/data}"
GW_URL="${MIDROUTE_GW_URL:-http://127.0.0.1:${MIDROUTE_GW_PORT:-8317}}"
ONEAPI_URL="${MIDROUTE_ONEAPI_URL:-http://127.0.0.1:${MIDROUTE_ONEAPI_PORT:-13000}}"
KEYS_FILE="$DATA/local.keys"
CHANNEL_NAME="midroute-local-gateway"

[ -f "$KEYS_FILE" ] || { echo "缺少 $KEYS_FILE，请先运行 run-local.sh" >&2; exit 1; }
GW_KEY="$(sed -n 's/^gateway_key=//p' "$KEYS_FILE")"
ONEAPI_ADMIN_TOKEN="$(sed -n 's/^oneapi_admin_token=//p' "$KEYS_FILE")"
[ -n "$GW_KEY" ] || { echo "local.keys 缺少 gateway_key" >&2; exit 1; }
[ -n "$ONEAPI_ADMIN_TOKEN" ] || { echo "local.keys 缺少 oneapi_admin_token" >&2; exit 1; }

models_json="$(curl -fsS --max-time 5 "$GW_URL/v1/models" -H "Authorization: Bearer $GW_KEY")"
models="$(MODELS_JSON="$models_json" python3 - <<'PY'
import json, os
try:
    payload = json.loads(os.environ.get("MODELS_JSON", "{}"))
    ids = []
    for item in payload.get("data", []):
        model_id = item.get("id") if isinstance(item, dict) else None
        if model_id and model_id not in ids:
            ids.append(model_id)
    print(",".join(ids))
except (TypeError, ValueError):
    print("")
PY
)"

if [ -z "$models" ]; then
  echo "OneAPI 渠道同步跳过：本地网关当前没有可注册模型"
  exit 0
fi

channels_json="$(curl -fsS --max-time 5 "$ONEAPI_URL/api/channel/?p=0" -H "Authorization: Bearer $ONEAPI_ADMIN_TOKEN")"
channel_id="$(CHANNELS_JSON="$channels_json" CHANNEL_NAME="$CHANNEL_NAME" python3 - <<'PY'
import json, os
try:
    payload = json.loads(os.environ.get("CHANNELS_JSON", "{}"))
    for item in payload.get("data", []) or []:
        if item.get("name") == os.environ.get("CHANNEL_NAME"):
            print(item.get("id", ""))
            break
except (TypeError, ValueError):
    pass
PY
)"

payload="$(GW_KEY="$GW_KEY" GW_URL="$GW_URL" MODELS="$models" CHANNEL_NAME="$CHANNEL_NAME" python3 - <<'PY'
import json, os
print(json.dumps({
    "type": 42,
    "key": os.environ["GW_KEY"],
    "status": 1,
    "name": os.environ["CHANNEL_NAME"],
    "base_url": os.environ["GW_URL"].rstrip("/") + "/v1",
    "models": os.environ["MODELS"],
    "group": "default",
    "weight": 1,
    "priority": 0,
    "config": "{}"
}))
PY
)"

if [ -n "$channel_id" ]; then
  curl -fsS --max-time 5 -o /dev/null -X PUT "$ONEAPI_URL/api/channel/" \
    -H "Authorization: Bearer $ONEAPI_ADMIN_TOKEN" -H "Content-Type: application/json" \
    --data "$payload"
  echo "OneAPI 本地网关渠道已更新（模型列表已同步）"
else
  curl -fsS --max-time 5 -o /dev/null -X POST "$ONEAPI_URL/api/channel/" \
    -H "Authorization: Bearer $ONEAPI_ADMIN_TOKEN" -H "Content-Type: application/json" \
    --data "$payload"
  echo "OneAPI 本地网关渠道已注册（模型列表已同步）"
fi
