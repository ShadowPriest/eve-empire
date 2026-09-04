# Собирает полный пакет деплоя в dist\deploy-package: свежий образ, команды
# RouterOS с подставленными секретами, консистентные снимки обеих баз и
# инструкцию. Папку целиком уносим на флешке к роутеру.
#
#   .\deploy\package.ps1
#   .\deploy\package.ps1 -Disk /usb1-part1 -Callback http://172.20.20.1:8080/callback
#   .\deploy\package.ps1 -NoData          # только образ и команды, без баз (270 МБ)

param(
    [string]$Disk     = '/usb1',
    [string]$Callback = 'http://172.20.20.1:8080/callback',
    [switch]$NoData
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)

$pkg = 'dist\deploy-package'
if (Test-Path $pkg) { Remove-Item -Recurse -Force $pkg }
New-Item -ItemType Directory -Force $pkg | Out-Null

Write-Host "`n=== 1/5 образ ===" -ForegroundColor Cyan
& .\deploy\build-arm64.ps1

Write-Host "`n=== 2/5 команды RouterOS ===" -ForegroundColor Cyan
& .\deploy\ros-commands.ps1 -Disk $Disk -Callback $Callback

Write-Host "`n=== 3/5 базы ===" -ForegroundColor Cyan
if ($NoData) {
    Write-Host 'пропущено (-NoData): базы переносим отдельно'
} else {
    # VACUUM INTO вместо копирования: сворачивает WAL и держит read-lock,
    # поэтому снимок консистентен даже с работающим dev-сервером.
    foreach ($db in 'sde.db', 'eve-empire.db') {
        if (-not (Test-Path $db)) { throw "нет $db в корне проекта" }
        go run ./cmd/dbsnapshot -db $db -out "$pkg\data\$db"
        if ($LASTEXITCODE -ne 0) { throw "снимок $db не удался" }
    }
}

Write-Host "`n=== 4/5 сборка папки ===" -ForegroundColor Cyan
Copy-Item dist\eve-empire-arm64.tar $pkg\
Copy-Item dist\ros-eve.rsc          $pkg\

$doc = @"
# Пакет деплоя EVE Empire → RB5009 (ShadowGW)

Собран ``deploy\package.ps1``. Диск ``$Disk``, callback ``$Callback``.

| файл | куда на роутере |
|---|---|
| ``eve-empire-arm64.tar`` | ``$Disk/eve-empire-arm64.tar`` |
| ``data\sde.db`` | ``$Disk/eve/data/sde.db`` |
| ``data\eve-empire.db`` | ``$Disk/eve/data/eve-empire.db`` |
| ``ros-eve.rsc`` | никуда, копируем содержимое в терминал |

Снимки баз сделаны через ``VACUUM INTO``: WAL уже свёрнут внутрь, файлов
``-wal``/``-shm`` рядом класть не надо и не нужно останавливать dev-сервер.

## 0. Проверить точку монтирования

``````
/disk print detail
``````

Если MOUNT-POINT не ``$Disk`` — пересобрать пакет: ``.\deploy\package.ps1 -Disk <точка>``.
Пути внутри уже подставлены, руками править ``.rsc`` не надо.

## 1. Файлы на диск

Флешка форматирована в ext4, Windows её не читает. Два пути:

- **физически с Linux-машины** — смонтировать, создать ``eve/data``, скопировать туда обе базы;
- **через WinBox → Files** — залить в корень диска (``sde.db`` 238 МБ пойдёт долго),
  каталоги при этом создаются заранее: ``/file add name=$Disk/eve type=directory``
  и ``/file add name=$Disk/eve/data type=directory``.

Тар кладём в корень диска, а не в NAND: ``$Disk/eve-empire-arm64.tar``.
Каталог ``$Disk/eve/root`` создастся сам при ``/container add``.

## 2. Команды

Открыть ``ros-eve.rsc``, скопировать целиком в терминал роутера. Там уже
подставлены Client ID, секрет и ключ шифрования из ``.env``. ``EVE_SCOPES``
там нет намеренно: список скоупов знает сам код, а переопределение длинным
набором ломает логин.

Дальше дождаться ``status=stopped`` (распаковка слоя ~минуту):

``````
/container print where name=eve-empire
``````

и стартовать:

``````
/container start [find name=eve-empire]
/log print where topics~"container"
``````

## 3. Проверка

Открыть ``http://172.20.20.1:8080``. Логин в EVE заработает только если в
приложении на developers.eveonline.com прописан callback ``$Callback``.
Ключ шифрования тот же, что на ПК, поэтому refresh-токены из перенесённой
базы расшифруются и заново логиниться не придётся.

## 3.1 Что нового в этой сборке

Появился **учёт ТМЦ и балансов** — раздел «Учёт» в меню.

Главное для эксплуатации: приложение теперь **собирает данные в фоне**. Планировщик
поднимает шесть задач (контракты каждые 5 минут, кошелёк, ордера и имущество раз в
час, работы раз в полчаса, чертежи раз в шесть часов). Ради этого прод и держат
включённым: ESI отдаёт кошельки и контракты скользящим окном и забывает их.

Следствия:

- **база начнёт расти** — прибавились история кошелька, ордера, работы, контракты,
  снимки имущества и сам реестр;
- **таблицы создадутся сами** при первом старте, миграции руками делать не надо;
- в логе при старте будет строка ``сбор: N альтов с чужими токенами пропущено`` —
  это нормально, речь о персонажах, чей токен выписан другим приложением EVE;
- если у альтов нет права ``esi-contracts...``, сборщик молча их пропустит и напишет
  об этом один раз. Контракты начнут собираться после перелогина через ``/reauth``.

Реестр на роутере будет пустым: он строится из собранной истории. Кнопка
«Провести инвентаризацию империи» на странице «Учёт» заведёт начальные остатки по
текущему имуществу — но жать её осмысленно не сразу, а когда сборщик отработает
хотя бы один полный круг (около пяти минут после старта).

## 4. Обновление на новую сборку

``````
/container stop [find name=eve-empire]
/container print where name=eve-empire
/container remove [find name=eve-empire]
``````

залить новый тар поверх старого и повторить из ``ros-eve.rsc`` **только**
блок ``/container add`` — envs, mounts, veth и данные на диске переживают
пересборку. Базы в ``$Disk/eve/data`` не трогаем: у прода свой набор
залогиненных персонажей, и снимок с ПК его затрёт.

## 5. Откат

Старый тар не удалять до проверки: ``/container remove`` сносит и
``root-dir``, но не ``$Disk/eve/data``. Вернуться на предыдущую сборку —
это ``/container add`` с прежним файлом.

## Если не поднялось

Полная таблица симптомов — в ``deploy\README.md`` репозитория. Три частых:

- сразу глохнет, в логе про ``EVE_CLIENT_ID`` — не подхватился ``envlists=eve``
  (проверить ``/container print detail``);
- «статическая база /data/sde.db не найдена» — не смонтировался ``mountlists``
  или базы лежат не в ``$Disk/eve/data``;
- пустые страницы и TLS в логе — нет DNS или выхода наружу из контейнера.
"@
[System.IO.File]::WriteAllText((Join-Path (Get-Location).Path "$pkg\DEPLOY.md"), $doc, (New-Object System.Text.UTF8Encoding $false))

Write-Host "`n=== 5/5 контрольные суммы ===" -ForegroundColor Cyan
$files = Get-ChildItem -Recurse -File $pkg | Where-Object { $_.Name -ne 'checksums.txt' }
$lines = foreach ($f in $files) {
    $rel = $f.FullName.Substring((Resolve-Path $pkg).Path.Length + 1)
    "{0}  {1}" -f (Get-FileHash $f.FullName -Algorithm SHA256).Hash.ToLower(), $rel
}
$lines | Out-File -Encoding ascii "$pkg\checksums.txt"

Write-Host ''
Get-ChildItem -Recurse -File $pkg |
    Select-Object @{n = 'файл'; e = { $_.FullName.Substring((Resolve-Path $pkg).Path.Length + 1) } },
                  @{n = 'МБ'; e = { [math]::Round($_.Length / 1MB, 1) } } |
    Format-Table -AutoSize
$total = (Get-ChildItem -Recurse -File $pkg | Measure-Object -Property Length -Sum).Sum
Write-Host ("пакет: {0} ({1:N0} МБ)" -f (Resolve-Path $pkg).Path, ($total / 1MB))
