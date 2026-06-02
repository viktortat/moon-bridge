#!/usr/bin/env bash
# Обратная совместимость — запускает deepseek-v4-flash.
# Используйте напрямую: start-deepseek-flash.sh или start-qwen.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$SCRIPT_DIR/start-deepseek-flash.sh" "$@"
