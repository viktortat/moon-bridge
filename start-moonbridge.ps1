# Переключение Codex на DeepSeek + запуск Moon Bridge
$MoonBridgeDir = $PSScriptRoot
$MoonBridgeExe = "$MoonBridgeDir\moonbridge.exe"
$MBConfig      = "$MoonBridgeDir\config.yml"
$CodexHome     = "$env:USERPROFILE\.codex"
$DeepSeekConf  = "$MoonBridgeDir\config.toml.deepseek"
$OpenAIBackup  = "$CodexHome\config.toml.bak"
$ActiveConf    = "$CodexHome\config.toml"

# Проверяем, не запущен ли уже Moon Bridge
$alreadyRunning = $false
try {
    Invoke-WebRequest -Uri "http://127.0.0.1:38440/v1/models" -UseBasicParsing -TimeoutSec 1 -ErrorAction Stop | Out-Null
    $alreadyRunning = $true
} catch {}

if ($alreadyRunning) {
    Write-Host "Moon Bridge already running on port 38440"
} else {
    # Сохраняем текущий конфиг как OpenAI-бэкап
    Copy-Item $ActiveConf $OpenAIBackup -Force
    Write-Host "Saved current config as config.toml.bak (OpenAI backup)"

    # Запускаем Moon Bridge
    Write-Host "Starting Moon Bridge..."
    Start-Process -FilePath $MoonBridgeExe -ArgumentList "--config", $MBConfig -WindowStyle Minimized

    # Ждём старта (до 10 сек)
    $started = $false
    for ($i = 1; $i -le 10; $i++) {
        Start-Sleep -Seconds 1
        try {
            Invoke-WebRequest -Uri "http://127.0.0.1:38440/v1/models" -UseBasicParsing -TimeoutSec 1 -ErrorAction Stop | Out-Null
            $started = $true
            break
        } catch {}
    }

    if (-not $started) {
        Write-Host "ERROR: Moon Bridge failed to start"
        exit 1
    }
    Write-Host "Moon Bridge started successfully."
}

# Применяем DeepSeek конфиг (всегда, чтобы режим точно был активен)
Copy-Item $DeepSeekConf $ActiveConf -Force
Write-Host "Config switched to DeepSeek (moonbridge). Codex is ready."
