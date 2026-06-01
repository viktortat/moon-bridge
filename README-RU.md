# Codex + DeepSeek через Moon Bridge

## Как это работает

```
Codex → Moon Bridge (127.0.0.1:38440) → DeepSeek API
```

Moon Bridge — локальный прокси-сервер, который переводит запросы Codex (формат OpenAI Responses API) в формат DeepSeek Anthropic API.

---

## Запуск Codex с DeepSeek

### Шаг 1 — запустить Moon Bridge

Windows (PowerShell):
```powershell
e:\_Projects3\25\codex-ds-test2\moon-bridge\start-moonbridge.ps1
```

Linux / macOS (bash):
```bash
bash e:/_Projects3/25/codex-ds-test2/moon-bridge/start-moonbridge.sh
# или, сделав исполняемым:
chmod +x e:/_Projects3/25/codex-ds-test2/moon-bridge/start-moonbridge.sh
e:/_Projects3/25/codex-ds-test2/moon-bridge/start-moonbridge.sh
```

Оба скрипта проверяют, не запущен ли Moon Bridge уже, и стартуют его в фоне. На Linux/macOS логи пишутся в `moonbridge.log` рядом со скриптом.

Альтернативно — запустить вручную в отдельном терминале (видны логи):

```powershell
e:\_Projects3\25\codex-ds-test2\moon-bridge\moonbridge.exe --config e:\_Projects3\25\codex-ds-test2\moon-bridge\config.yml
```

### Шаг 2 — запустить Codex

```powershell
cd <папка-проекта>
codex
```

Codex автоматически использует модель `deepseek-v4-flash` через Moon Bridge.

### Проверка соединения

```powershell
Invoke-WebRequest -Uri "http://127.0.0.1:38440/v1/models" -UseBasicParsing | Select-Object -ExpandProperty Content
```

Если Moon Bridge работает, вернётся список моделей DeepSeek.

---

## Смена модели DeepSeek

В файле [config.yml](./config.yml) в секции `routes.moonbridge` измените `model`:

```yaml
routes:
  moonbridge:
    model: deepseek-v4-pro   # или deepseek-v4-flash
    provider: deepseek
```

После изменения пересоздайте конфиг Codex:

```powershell
$CODEX_HOME = "$env:USERPROFILE\.codex"
$MODEL = e:\_Projects3\25\codex-ds-test2\moon-bridge\moonbridge.exe --config e:\_Projects3\25\codex-ds-test2\moon-bridge\config.yml --print-codex-model
e:\_Projects3\25\codex-ds-test2\moon-bridge\moonbridge.exe `
  --config e:\_Projects3\25\codex-ds-test2\moon-bridge\config.yml `
  --print-codex-config "$MODEL" `
  --codex-base-url "http://127.0.0.1:38440/v1" `
  --codex-home "$CODEX_HOME" `
  | Set-Content -Path "$CODEX_HOME\config.toml"
```

---

## Запуск Codex App (десктоп)

Codex App использует тот же `~/.codex/config.toml` — дополнительных настроек не требуется.

```powershell
# 1. Запустить Moon Bridge
e:\_Projects3\25\codex-ds-test2\moon-bridge\start-moonbridge.ps1

# 2. Открыть Codex App
codex app
# или сразу с папкой проекта:
codex app e:\_Projects3\25\codex-ds-test2
```

При первом запуске `codex app` скачает и установит десктопное приложение автоматически.

> Если App запущен, а Moon Bridge не работает — запросы упадут с ошибкой соединения.  
> Решение: перезапустить `start-moonbridge.ps1`.

---

## Возврат к стандартному режиму Codex (OpenAI)

Бэкап конфига сохранён в `~/.codex/config.toml.bak`.

Windows (PowerShell):
```powershell
e:\_Projects3\25\codex-ds-test2\moon-bridge\stop-moonbridge.ps1
```

Linux / macOS (bash):
```bash
bash e:/_Projects3/25/codex-ds-test2/moon-bridge/stop-moonbridge.sh
```

Скрипты останавливают Moon Bridge и восстанавливают стандартный конфиг. Codex вернётся к моделям OpenAI (gpt-*, o3 и т.д.).

---

## Переключение между режимами вручную

Если нужно часто переключаться, удобно держать два файла:

| Файл | Назначение |
|------|-----------|
| `~/.codex/config.toml.bak` | Стандартный Codex (OpenAI) |
| `~/.codex/config.toml` | Текущий активный конфиг |

Переключение на DeepSeek:
```powershell
Copy-Item "$env:USERPROFILE\.codex\config.toml" "$env:USERPROFILE\.codex\config.toml.openai" -Force
Copy-Item "e:\_Projects3\25\codex-ds-test2\moon-bridge\config-codex.toml" "$env:USERPROFILE\.codex\config.toml" -Force
```

Переключение на OpenAI:
```powershell
Copy-Item "$env:USERPROFILE\.codex\config.toml.bak" "$env:USERPROFILE\.codex\config.toml" -Force
```

---

## Структура файлов

```
e:\_Projects3\25\codex-ds-test2\moon-bridge\
├── moonbridge.exe          # скомпилированный прокси
├── config.yml              # конфигурация Moon Bridge (ключ DeepSeek, модели)
├── start-moonbridge.ps1    # запуск Moon Bridge + DeepSeek (Windows PowerShell)
├── start-moonbridge.sh     # запуск Moon Bridge + DeepSeek (Linux / macOS)
├── stop-moonbridge.ps1     # возврат к OpenAI (Windows PowerShell)
├── stop-moonbridge.sh      # возврат к OpenAI (Linux / macOS)
└── README-RU.md            # эта инструкция

C:\Users\viktor\.codex\
├── config.toml             # активный конфиг Codex (сейчас → DeepSeek)
├── config.toml.bak         # бэкап стандартного конфига Codex (OpenAI)
└── models_catalog.json     # метаданные моделей (сгенерирован Moon Bridge)
```

---

## Ключ DeepSeek

API-ключ хранится в `config.yml` в секции `providers.deepseek.api_key`.  
При необходимости обновить ключ — отредактировать только это поле.
