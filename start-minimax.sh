#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOONBRIDGE_BIN="$SCRIPT_DIR/moonbridge"
MB_CONFIG_TPL="$SCRIPT_DIR/config_MM.yml"
MB_CONFIG="$SCRIPT_DIR/.config-active.yml"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
MODEL_CONF="$SCRIPT_DIR/config.toml.minimax"
ACTIVE_CONF="$CODEX_HOME/config.toml"

if [ ! -f "$MOONBRIDGE_BIN" ] && [ -f "${MOONBRIDGE_BIN}.exe" ]; then
    MOONBRIDGE_BIN="${MOONBRIDGE_BIN}.exe"
fi

# Загружаем .env
if [ -f "$SCRIPT_DIR/.env" ]; then
    set -a; source "$SCRIPT_DIR/.env"; set +a
else
    echo "ERROR: $SCRIPT_DIR/.env not found"; exit 1
fi

# Подставляем ключи в конфиг
envsubst < "$MB_CONFIG_TPL" > "$MB_CONFIG"

server_running() {
    curl -sf --connect-timeout 1 "http://127.0.0.1:38440/v1/models" -o /dev/null 2>/dev/null
}

kill_moonbridge() {
    powershell.exe -Command "Get-Process moonbridge -ErrorAction SilentlyContinue | Stop-Process -Force" 2>/dev/null || true
    for i in $(seq 1 10); do
        sleep 1
        if ! server_running; then return 0; fi
    done
}

if server_running; then
    echo "Stopping existing Moon Bridge..."
    kill_moonbridge
fi

# OpenRouter делает проверки при старте (~40 сек), ждём дольше
echo "Starting Moon Bridge (minimax/minimax-m3 via OpenRouter)..."
nohup "$MOONBRIDGE_BIN" --config "$MB_CONFIG" > "$SCRIPT_DIR/moonbridge.log" 2>&1 &

started=false
for i in $(seq 1 60); do
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

cp "$MODEL_CONF" "$ACTIVE_CONF"
echo "Moon Bridge started. Config switched to MiniMax-M3 (OpenRouter). Codex is ready."
