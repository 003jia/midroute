#!/usr/bin/env bash
set -euo pipefail
# Midroute 数据备份（ISS-04 修复版）。
# - 覆盖现行主库 midroute.db（sqlite3 在线备份，保证 WAL 一致性）
# - 默认排除明文凭据（local.keys、data.key）；--include-secrets 显式携带
# - manifest 记录应用版本、schema 版本与 SHA-256 校验和
# 用法: ./build/backup-data.sh [--include-secrets] [目标目录]
# 默认目标: ./backups/backup-YYYYMMDD-HHMMSS/
# 注意：恢复入口与损坏包/错误口令测试属 MR-028 后续步骤，本脚本只负责备份侧。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../" && pwd)"
SRC="$ROOT/data"
INCLUDE_SECRETS=0
for arg in "$@"; do
  case "$arg" in
    --include-secrets) INCLUDE_SECRETS=1 ;;
    -*) echo "未知选项: $arg" >&2; exit 2 ;;
    *) DEST_ARG="$arg" ;;
  esac
done

if [ ! -d "$SRC" ]; then
  echo "data/ 不存在，跳过备份（无运行数据）"
  exit 0
fi

DEST="${DEST_ARG:-$ROOT/backups/backup-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$DEST"

echo "==> 备份 data/ -> $DEST"

# SQLite 一致性备份：必须用 sqlite3 在线备份，不得复制运行中 WAL 文件。
# 备份产物可能继承 WAL 模式头并生成空边车文件，复制后清理。
backup_sqlite() { # db 文件名
  local db="$SRC/$1"
  [ -f "$db" ] || return 1
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$db" ".backup '$DEST/$1'"
    rm -f "$DEST/$1-wal" "$DEST/$1-shm"
    echo "  $1: sqlite3 在线备份完成"
  else
    echo "  错误：缺少 sqlite3，拒绝文件复制运行中的 WAL 数据库（$1）" >&2
    exit 1
  fi
}

# 现行主库（Midroute）
backup_sqlite "midroute.db" || echo "  midroute.db: 不存在，跳过"
# 旧版遗留库（历史数据兼容）
backup_sqlite "usage.sqlite" || true

# 非凭据配置/数据只读复制
for f in gateway.local.yaml static usage-imports oneapi; do
  [ -e "$SRC/$f" ] || continue
  cp -Rp "$SRC/$f" "$DEST/"
done

# 凭据文件：默认排除；显式 --include-secrets 才携带（chmod 600 + 醒目警告）
if [ "$INCLUDE_SECRETS" = 1 ]; then
  for f in local.keys data.key; do
    [ -e "$SRC/$f" ] || continue
    cp -p "$SRC/$f" "$DEST/"
    chmod 600 "$DEST/$f"
  done
  echo "  !!! 已包含明文凭据（local.keys/data.key）：请立即移至加密存储" >&2
else
  echo "  已排除明文凭据（local.keys/data.key）；换机恢复后需重新授权（详见 README）"
fi

# schema 版本（从主库 migration 表读取）
SCHEMA_VERSION="unknown"
if [ -f "$DEST/midroute.db" ] && command -v sqlite3 >/dev/null 2>&1; then
  SCHEMA_VERSION="$(sqlite3 "$DEST/midroute.db" 'SELECT COALESCE(MAX(version),0) FROM schema_migrations;' 2>/dev/null || echo unknown)"
fi
APP_VERSION="$(cat "$ROOT/VERSION" 2>/dev/null || echo unknown)"

# 清单：应用版本、schema 版本、逐文件 SHA-256
{
  echo "midroute data backup"
  echo "created: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "source: $SRC"
  echo "app_version: $APP_VERSION"
  echo "schema_version: $SCHEMA_VERSION"
  echo "includes_secrets: $INCLUDE_SECRETS"
  echo "files:"
  ( cd "$DEST" && find . -type f ! -name BACKUP-MANIFEST.txt -maxdepth 3 | sort | while read -r f; do
      printf '  %s  sha256=%s\n' "${f#./}" "$(shasum -a 256 "$f" | cut -d" " -f1)"
    done )
} > "$DEST/BACKUP-MANIFEST.txt"

chmod 700 "$DEST"
echo "==> 备份完成: ${DEST}（清单含版本与校验和；不含密钥时恢复后需重新授权）"
