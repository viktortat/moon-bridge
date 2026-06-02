#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
OPENAI_BACKUP="E:/_Projects3/20/codex-deepseek-bridge/config.toml.bak"
ACTIVE_CONF="$CODEX_HOME/config.toml"
ACTIVE_MB_CONFIG="$SCRIPT_DIR/.active-mb-config"

# Останавливаем Moon Bridge
powershell.exe -Command "Get-Process moonbridge -ErrorAction SilentlyContinue | Stop-Process -Force" 2>/dev/null && echo "Moon Bridge stopped." || echo "Moon Bridge is not running."

# Сбрасываем флаг активного конфига
rm -f "$ACTIVE_MB_CONFIG"

# Восстанавливаем OpenAI конфиг из фиксированного бэкапа
if [ -f "$OPENAI_BACKUP" ]; then
    cp "$OPENAI_BACKUP" "$ACTIVE_CONF"
    echo "Config restored from $OPENAI_BACKUP"
else
    echo "ERROR: Backup not found at $OPENAI_BACKUP"
    exit 1
fi

echo "Done. Codex is back to standard OpenAI mode."
