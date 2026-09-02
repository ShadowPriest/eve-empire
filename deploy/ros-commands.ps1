# Генерирует dist\ros-eve.rsc — команды RouterOS с подставленными значениями из .env.
# Секреты в чат не попадают, файл открываем и копируем в терминал роутера.
#
#   .\deploy\ros-commands.ps1 [-Disk /usb1] [-Callback http://172.20.20.1:8080/callback]

param(
    [string]$Disk     = '/usb1',                                # mount-point раздела (/disk print)
    [string]$Callback = 'http://172.20.20.1:8080/callback',     # адрес роутера в LAN
    [string]$TarName  = 'eve-empire-arm64.tar'                  # имя файла тара на роутере
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)

# .env — дев-приложение, поверх него обязательно ложится .env.prod с прод-client_id.
# Без этой перекладки на роутер уехало бы дев-приложение, и прод потерял бы токены.
$env_ = @{}
foreach ($file in '.env', '.env.prod') {
    if (-not (Test-Path $file)) { throw "нет файла $file" }
    foreach ($line in Get-Content $file) {
        if ($line -match '^\s*([A-Z_]+)\s*=\s*(.*)$') { $env_[$matches[1]] = $matches[2].Trim() }
    }
}
foreach ($k in 'EVE_CLIENT_ID', 'EVE_CLIENT_SECRET', 'ENCRYPTION_KEY') {
    if (-not $env_[$k]) { throw "не задан $k (.env / .env.prod)" }
}

# RouterOS: значение в кавычках, экранируются " и \
function Q([string]$v) { '"' + ($v -replace '\\', '\\\\' -replace '"', '\\"') + '"' }

# EVE_SCOPES здесь намеренно нет: список скоупов знает сам код
# (config.defaultScopes), а переопределение длинным набором ломает логин.
$envs = [ordered]@{
    EVE_CLIENT_ID     = $env_['EVE_CLIENT_ID']
    EVE_CLIENT_SECRET = $env_['EVE_CLIENT_SECRET']
    ENCRYPTION_KEY    = $env_['ENCRYPTION_KEY']
    EVE_CALLBACK_URL  = $Callback
    ESI_USER_AGENT    = if ($env_['ESI_USER_AGENT']) { $env_['ESI_USER_AGENT'] } else { 'eve-empire/0.1' }
    DB_PATH           = '/data/eve-empire.db'
    SDE_PATH          = '/data/sde.db'
    LISTEN_ADDR       = ':8080'
    TZ                = 'Asia/Novosibirsk'
}

$out = New-Object System.Collections.Generic.List[string]
$out.Add('# EVE Empire → RB5009. Сгенерировано deploy\ros-commands.ps1')
$out.Add("# диск $Disk, callback $Callback")
$out.Add('')
$out.Add('# 1. Переменные окружения (список eve)')
$out.Add('/container envs remove [find list=eve]')
foreach ($k in $envs.Keys) {
    if (-not $envs[$k]) { continue }
    $out.Add("/container envs add list=eve key=$k value=$(Q $envs[$k])")
}
$out.Add('')
$out.Add('# 2. Монтирование данных (sde.db и рабочая база лежат на диске)')
$out.Add("/container mounts add list=eve-data src=$Disk/eve/data dst=/data")
$out.Add('')
$out.Add('# 3. Контейнер. Свойства во множественном числе — это 7.21+')
$out.Add("/container add file=$Disk/$TarName interface=veth-eve root-dir=$Disk/eve/root \")
$out.Add('    mountlists=eve-data envlists=eve workdir=/data entrypoint=/app/eve-empire \')
$out.Add('    dns=8.8.8.8,1.1.1.1 logging=yes start-on-boot=yes name=eve-empire')
$out.Add('/container print where name=eve-empire')
$out.Add('/container start [find name=eve-empire]')

New-Item -ItemType Directory -Force dist | Out-Null

# Без BOM: RouterOS спотыкается о него в первой строке при /import.
$text = ($out -join "`r`n") + "`r`n"
$dest = Join-Path (Get-Location).Path 'dist\ros-eve.rsc'
[System.IO.File]::WriteAllText($dest, $text, (New-Object System.Text.UTF8Encoding $false))
Write-Host "готово: dist\ros-eve.rsc ($($out.Count) строк)"
