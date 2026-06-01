#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
DEEPSEEK_CONF="$SCRIPT_DIR/config.toml.deepseek"
OPENAI_BACKUP="$CODEX_HOME/config.toml.bak"
ACTIVE_CONF="$CODEX_HOME/config.toml"

# Сохраняем текущий DeepSeek конфиг (чтобы start мог его восстановить)
if grep -q "moonbridge" "$ACTIVE_CONF" 2>/dev/null; then
    cp "$ACTIVE_CONF" "$DEEPSEEK_CONF"
    echo "Saved current DeepSeek config to config.toml.deepseek"
fi

# Останавливаем Moon Bridge
if pgrep -x "moonbridge" &>/dev/null 2>&1 || pgrep -x "moonbridge.exe" &>/dev/null 2>&1; then
    pkill -x "moonbridge" 2>/dev/null || pkill -x "moonbridge.exe" 2>/dev/null || true
    echo "Moon Bridge stopped."
else
    echo "Moon Bridge is not running."
fi

# Восстанавливаем OpenAI конфиг
if [ -f "$OPENAI_BACKUP" ]; then
    cp "$OPENAI_BACKUP" "$ACTIVE_CONF"
    echo "Config restored to standard OpenAI mode."
else
    echo "ERROR: OpenAI backup not found at $OPENAI_BACKUP"
    exit 1
fi

echo "Done. Codex is back to standard OpenAI mode."
