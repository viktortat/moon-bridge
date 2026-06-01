# Возврат Codex к стандартному режиму (OpenAI)
$MoonBridgeDir = $PSScriptRoot
$CodexHome     = "$env:USERPROFILE\.codex"
$DeepSeekConf  = "$MoonBridgeDir\config.toml.deepseek"
$OpenAIBackup  = "$CodexHome\config.toml.bak"
$ActiveConf    = "$CodexHome\config.toml"

# Сохраняем текущий DeepSeek конфиг (чтобы start мог его восстановить)
if (Select-String -Path $ActiveConf -Pattern "moonbridge" -Quiet) {
    Copy-Item $ActiveConf $DeepSeekConf -Force
    Write-Host "Saved current DeepSeek config to config.toml.deepseek"
}

# Останавливаем Moon Bridge
$proc = Get-Process -Name "moonbridge" -ErrorAction SilentlyContinue
if ($proc) {
    Stop-Process -Name "moonbridge" -Force
    Write-Host "Moon Bridge stopped."
} else {
    Write-Host "Moon Bridge is not running."
}

# Восстанавливаем OpenAI конфиг
if (Test-Path $OpenAIBackup) {
    Copy-Item $OpenAIBackup $ActiveConf -Force
    Write-Host "Config restored to standard OpenAI mode."
} else {
    Write-Host "ERROR: OpenAI backup not found at $OpenAIBackup"
    exit 1
}

Write-Host "Done. Codex is back to standard OpenAI mode."
