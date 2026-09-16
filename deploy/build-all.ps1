# Собирает все дистрибутивы EVE Empire за один проход в dist\release:
#
#   eve-empire-<v>-mikrotik.zip         образ linux/arm64 (docker save) + ros-eve.rsc + README
#   eve-empire-<v>-windows-amd64.zip    eve-empire.exe, sdeimport.exe, .env.example, SETUP.md
#   eve-empire_<v>_amd64.deb            служба systemd для Debian/Ubuntu (cmd/mkdeb)
#   SHA256SUMS
#
#   .\deploy\build-all.ps1
#   .\deploy\build-all.ps1 -Version 0.2.0
#   .\deploy\build-all.ps1 -DebArch amd64,arm64          # deb и под ARM-машины
#   .\deploy\build-all.ps1 -NoMikrotik -NoWindows        # только deb
#
# Docker, dpkg-deb и ar не нужны: modernc SQLite — чистый Go, поэтому все
# бинарники статические (CGO_ENABLED=0), образ пакует cmd/mkimage, deb — cmd/mkdeb.
# Версия по умолчанию — 0.1.<число коммитов>+g<sha>, при незакоммиченных
# правках добавляется .dirty. Пакет для роутера с базами (когда прод ставится
# с нуля) по-прежнему собирает deploy\package.ps1.

param(
    [string]$Version  = '',
    [string]$OutDir   = 'dist\release',
    [string]$Disk     = '/usb1',                                # для ros-eve.rsc
    [string]$Callback = 'http://172.20.20.1:8080/callback',     # для ros-eve.rsc
    [string[]]$DebArch = @('amd64'),
    [switch]$NoMikrotik,
    [switch]$NoWindows,
    [switch]$NoDeb
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)
$root = (Get-Location).Path
Add-Type -AssemblyName System.IO.Compression.FileSystem

# --- версия ---------------------------------------------------------------
if (-not $Version) {
    $count = (git rev-list --count HEAD).Trim()
    $sha   = (git rev-parse --short HEAD).Trim()
    $Version = "0.1.$count+g$sha"
    if (git status --porcelain) { $Version += '.dirty' }
}
# Debian: версия начинается с цифры, допустимы буквы, цифры . + - ~
if ($Version -notmatch '^[0-9][A-Za-z0-9.+~-]*$') { throw "недопустимая версия для deb: $Version" }

# --- вспомогательное ------------------------------------------------------
function Step([string]$title) { Write-Host "`n=== $title ===" -ForegroundColor Cyan }

function Build-Go([string]$os, [string]$arch, [string]$pkg, [string]$out) {
    $env:GOOS = $os; $env:GOARCH = $arch; $env:CGO_ENABLED = '0'
    try {
        go build -trimpath -ldflags '-s -w' -o $out $pkg
        if ($LASTEXITCODE -ne 0) { throw "go build $pkg ($os/$arch) failed" }
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
    Write-Host ("  {0,-42} {1,7:N1} МБ" -f $out, ((Get-Item $out).Length / 1MB))
}

function New-Zip([string]$srcDir, [string]$zip) {
    if (Test-Path $zip) { Remove-Item $zip }
    [IO.Compression.ZipFile]::CreateFromDirectory((Resolve-Path $srcDir).Path, $zip,
        [IO.Compression.CompressionLevel]::Optimal, $false)
}

function Write-Utf8([string]$path, [string]$text) {
    [IO.File]::WriteAllText($path, $text, (New-Object Text.UTF8Encoding $false))
}

$stage = Join-Path $root 'dist\stage'
if (Test-Path $stage)  { Remove-Item -Recurse -Force $stage }
if (Test-Path $OutDir) { Remove-Item -Recurse -Force $OutDir }
New-Item -ItemType Directory -Force $stage, $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path

Write-Host "EVE Empire $Version -> $OutDir"
$built = @()

# --- 1. MikroTik ----------------------------------------------------------
if (-not $NoMikrotik) {
    Step '1/3 MikroTik (linux/arm64, образ docker save)'
    & .\deploy\build-arm64.ps1
    $dir = Join-Path $stage 'mikrotik'
    New-Item -ItemType Directory -Force $dir | Out-Null
    Copy-Item dist\eve-empire-arm64.tar $dir\

    # Команды RouterOS с подставленными секретами: только если есть .env.prod,
    # иначе пакет получается без секретов (для чужого роутера так и нужно).
    if ((Test-Path .env) -and (Test-Path .env.prod)) {
        & .\deploy\ros-commands.ps1 -Disk $Disk -Callback $Callback
        Copy-Item dist\ros-eve.rsc $dir\
        $rscNote = "``ros-eve.rsc`` — команды RouterOS с подставленными Client ID, секретом и ключом из ``.env``/``.env.prod``; копируем целиком в терминал роутера."
    } else {
        Write-Host 'нет .env/.env.prod: ros-eve.rsc не сгенерирован, команды собираем по deploy\README.md' -ForegroundColor Yellow
        $rscNote = "Команды RouterOS (envs, mounts, ``/container add``) — по разделам 5–6 ``deploy/README.md`` репозитория."
    }

    Write-Utf8 (Join-Path $dir 'README.md') @"
# EVE Empire $Version — MikroTik (RouterOS 7.21+, arm64)

| файл | куда |
|---|---|
| ``eve-empire-arm64.tar`` | ``$Disk/eve-empire-arm64.tar`` (корень диска, не NAND) |
| ``ros-eve.rsc`` | никуда: копируем содержимое в терминал |

$rscNote

Образ однослойный, entrypoint ``/app/eve-empire``, рабочий каталог ``/data``:
туда монтируется ``$Disk/eve/data`` с ``sde.db`` и ``eve-empire.db``. Баз в этом
пакете нет — при первой установке их переносит ``deploy\package.ps1``.

## Обновление на эту сборку

``````
/container stop [find name=eve-empire]
/container remove [find name=eve-empire]
``````

Залить тар поверх старого и повторить из ``ros-eve.rsc`` только блок
``/container add``: envs, mounts, veth и данные на диске переживают пересборку.
Затем ``/container start [find name=eve-empire]`` и ``/log print where topics~"container"``.

Полная инструкция, таблица симптомов и откат — ``deploy/README.md`` в репозитории.
"@
    $zip = Join-Path $OutDir "eve-empire-$Version-mikrotik.zip"
    New-Zip $dir $zip
    $built += $zip
}

# --- 2. Windows -----------------------------------------------------------
if (-not $NoWindows) {
    Step '2/3 Windows (amd64)'
    $dir = Join-Path $stage 'windows'
    New-Item -ItemType Directory -Force $dir | Out-Null
    Build-Go windows amd64 ./cmd/server    "$dir\eve-empire.exe"
    Build-Go windows amd64 ./cmd/sdeimport "$dir\sdeimport.exe"
    Copy-Item .env.example            $dir\.env.example
    Copy-Item deploy\windows\SETUP.md $dir\SETUP.md
    Copy-Item LICENSE                 $dir\LICENSE.txt
    $zip = Join-Path $OutDir "eve-empire-$Version-windows-amd64.zip"
    New-Zip $dir $zip
    $built += $zip
}

# --- 3. Debian / Ubuntu ---------------------------------------------------
if (-not $NoDeb) {
    foreach ($arch in $DebArch) {
        Step "3/3 Debian/Ubuntu ($arch)"
        $dir = Join-Path $stage "linux-$arch"
        New-Item -ItemType Directory -Force $dir | Out-Null
        Build-Go linux $arch ./cmd/server    "$dir\eve-empire"
        Build-Go linux $arch ./cmd/sdeimport "$dir\sdeimport"
        $deb = Join-Path $OutDir "eve-empire_${Version}_$arch.deb"
        go run ./cmd/mkdeb -bin "$dir\eve-empire" -sdeimport "$dir\sdeimport" `
            -version $Version -arch $arch -assets deploy\debian -out $deb
        if ($LASTEXITCODE -ne 0) { throw "mkdeb ($arch) failed" }
        $built += $deb
    }
}

# --- контрольные суммы и итог ---------------------------------------------
Step 'контрольные суммы'
$sums = foreach ($f in $built) {
    "{0}  {1}" -f (Get-FileHash $f -Algorithm SHA256).Hash.ToLower(), (Split-Path $f -Leaf)
}
Write-Utf8 (Join-Path $OutDir 'SHA256SUMS') (($sums -join "`n") + "`n")

Write-Host ''
Get-ChildItem -File $OutDir |
    Select-Object @{n = 'файл'; e = { $_.Name } }, @{n = 'МБ'; e = { [math]::Round($_.Length / 1MB, 1) } } |
    Format-Table -AutoSize
Write-Host "готово: $OutDir"
