#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOONBRIDGE_BIN="$SCRIPT_DIR/moonbridge"
MB_CONFIG="$SCRIPT_DIR/config.yml"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
DEEPSEEK_CONF="$SCRIPT_DIR/config.toml.deepseek"
OPENAI_BACKUP="$CODEX_HOME/config.toml.bak"
ACTIVE_CONF="$CODEX_HOME/config.toml"

# На Windows используем .exe
if [ ! -f "$MOONBRIDGE_BIN" ] && [ -f "${MOONBRIDGE_BIN}.exe" ]; then
    MOONBRIDGE_BIN="${MOONBRIDGE_BIN}.exe"
fi

# Проверка через HTTP
server_running() {
    curl -sf --connect-timeout 1 "http://127.0.0.1:38440/v1/models" -o /dev/null 2>/dev/null
}

if server_running; then
    echo "Moon Bridge already running on port 38440"
else
    # Сохраняем текущий конфиг как OpenAI-бэкап
    cp "$ACTIVE_CONF" "$OPENAI_BACKUP"
    echo "Saved current config as config.toml.bak (OpenAI backup)"

    # Запускаем Moon Bridge
    echo "Starting Moon Bridge..."
    nohup "$MOONBRIDGE_BIN" --config "$MB_CONFIG" > "$SCRIPT_DIR/moonbridge.log" 2>&1 &

    # Ждём старта (до 10 сек)
    started=false
    for i in $(seq 1 10); do
        sleep 1
        if server_running; then
            started=true
            break
        fi
    done

    if [ "$started" = false ]; then
        echo "ERROR: Moon Bridge failed to start. Check $SCRIPT_DIR/moonbridge.log"
        exit 1
    fi
    echo "Moon Bridge started successfully."
fi

# Применяем DeepSeek конфиг (всегда, чтобы режим точно был активен)
cp "$DEEPSEEK_CONF" "$ACTIVE_CONF"
echo "Config switched to DeepSeek (moonbridge). Codex is ready."
