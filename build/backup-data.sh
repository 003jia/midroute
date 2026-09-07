#!/usr/bin/env bash
set -euo pipefail
# 只读备份：备份 data/ 全量 + data.key，生成清单。不修改任何运行数据。
# 用法: ./build/backup-data.sh [目标目录]
# 默认目标: ./backups/backup-YYYYMMDD-HHMMSS/
# 注意: data/ 中如含明文凭据文件，备份后应另行加密存储；本脚本不落明文到备份清单之外。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/data"
if [ ! -d "$SRC" ]; then
  echo "data/ 不存在，跳过备份（无运行数据）"
  exit 0
fi

DEST="${1:-$ROOT/backups/backup-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$DEST"

echo "==> 备份 data/ -> $DEST"

# SQLite 一致性备份：用 sqlite3 在线备份优先，避免复制运行中 WAL 的不一致
if command -v sqlite3 >/dev/null 2>&1 && [ -f "$SRC/usage.sqlite" ]; then
  sqlite3 "$SRC/usage.sqlite" ".backup '$DEST/usage.sqlite'" 2>/dev/null \
    && echo "  usage.sqlite: sqlite3 在线备份完成" \
    || { echo "  usage.sqlite: sqlite3 备份失败，改用文件复制"; cp -p "$SRC/usage.sqlite" "$DEST/"; }
else
  cp -p "$SRC"/usage.sqlite* "$DEST/" 2>/dev/null || true
fi

# 其余文件只读复制（保留权限位）
for f in data.key local.keys gateway.local.yaml static usage-imports oneapi; do
  [ -e "$SRC/$f" ] || continue
  cp -Rp "$SRC/$f" "$DEST/"
done

# 生成清单
cat > "$DEST/BACKUP-MANIFEST.txt" <<EOF
midroute data backup
created: $(date -u +%Y-%m-%dT%H:%M:%SZ)
source: $SRC
files:
$(cd "$SRC" && find . -type f -maxdepth 3 | sort | sed 's/^/  /')
EOF

chmod 700 "$DEST"
echo "==> 备份完成: $DEST"
echo "    !!! 备份含 data.key 与可能的敏感文件，请将目录移至加密存储并设置 700 权限。"