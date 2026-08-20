# Сборка образа EVE Empire под Mikrotik RB5009 (linux/arm64) без Docker.
#
#   .\deploy\build-arm64.ps1
#
# На выходе dist\eve-empire-arm64.tar — его импортирует RouterOS
# (/container/add file=...). Подробности деплоя — deploy\README.md.

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)
New-Item -ItemType Directory -Force dist | Out-Null

# 1. Корневые сертификаты: в образе нет системного хранилища, а Go на Linux
#    ищет /etc/ssl/certs/ca-certificates.crt. Берём доверенные корни Windows.
$certs = 'dist\ca-certificates.crt'
$roots = Get-ChildItem Cert:\LocalMachine\Root
$pem = foreach ($c in $roots) {
    "-----BEGIN CERTIFICATE-----"
    [Convert]::ToBase64String($c.RawData, 'InsertLineBreaks')
    "-----END CERTIFICATE-----"
}
$pem -join "`n" | Out-File -Encoding ascii $certs
Write-Host "сертификаты: $certs ($($roots.Count) корней)"

# 2. Статический бинарник. modernc SQLite — чистый Go, CGO не нужен.
$env:GOOS = 'linux'; $env:GOARCH = 'arm64'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags '-s -w' -o dist/eve-empire ./cmd/server
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
Write-Host "бинарник:    dist\eve-empire ($([math]::Round((Get-Item dist\eve-empire).Length/1MB,1)) МБ)"

# 3. Образ в формате docker save.
go run ./cmd/mkimage -bin dist/eve-empire -certs $certs -out dist/eve-empire-arm64.tar -tag eve-empire:arm64
if ($LASTEXITCODE -ne 0) { throw 'mkimage failed' }
