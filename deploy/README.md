# Деплой EVE Empire на Mikrotik RB5009 (RouterOS 7.23.1, arm64)

Команды даны для **7.23.1** (2026-06-02). Важно, потому что имена свойств контейнера
менялись: в 7.21 `mounts` превратилось в `mountlists`, а `name=` в `/container/mounts`
— в `list=`. Старые гайды в сети (и почти все статьи) написаны под 7.1x и на 7.23
не примутся. Проверить на месте всегда можно табуляцией: `/container/add ?`.

## Что уже настроено на ShadowGW (сверено с export 2026-08-08)

Пункты 2 и 3 выполнены: пакет `container` стоит, device-mode разрешён (работает
контейнер `mihomo-ros`), `veth-eve` 172.17.0.2/24 в бридже `containers` (172.17.0.1/24),
srcnat masquerade для 172.17.0.0/24 и dst-nat 8080 из LAN на 172.17.0.2:8080 — всё есть.
LAN роутера — **172.20.20.1/24**, значит callback `http://172.20.20.1:8080/callback`.
Диск примонтирован как **`/usb1`** (не `usb1-part1` — это видно по рабочему mihomo:
`root-dir=/usb1/mihomo-root`), `tmpdir=/usb1/pull` уже задан.

Команды с подставленными значениями из `.env` генерирует
[ros-commands.ps1](ros-commands.ps1) → `dist\ros-eve.rsc` (секреты остаются в файле).

## 1. Сборка образа на ПК

```powershell
.\deploy\build-arm64.ps1
```

Docker не нужен: modernc SQLite — чистый Go, поэтому `GOOS=linux GOARCH=arm64
CGO_ENABLED=0` даёт статический бинарник, а `cmd/mkimage` пакует его в тар формата
`docker save` (однослойный образ, «legacy»-раскладка — её понимают и Docker, и
контейнерный движок RouterOS).

На выходе `dist\eve-empire-arm64.tar` (~13 МБ). Внутри:

| путь | что |
|---|---|
| `/app/eve-empire` | бинарник, entrypoint |
| `/etc/ssl/certs/ca-certificates.crt` | корневые сертификаты (экспорт `Cert:\LocalMachine\Root`) |
| `/etc/resolv.conf` | фолбэк-резолвер, RouterOS перезапишет своим при `dns=` |
| `/data`, `/tmp` | точки монтирования |

Рабочий каталог — `/data`, порт 8080. Шаблоны вшиты в бинарник, часовые пояса тоже
(`time/tzdata`), так что образ самодостаточен.

**Баз данных в образе нет.** `sde.db` — 250 МБ, рабочая база растёт и пишется; обе
живут на USB-диске и монтируются в `/data`.

## 2. Один раз: пакет container и device-mode

1. Скачать `container-7.23.1-arm64.npk` — он лежит в архиве Extra packages **ровно той
   же версии**, что RouterOS (mikrotik.com/download → 7.23.1 → ARM64 → Extra packages).
   Распаковать zip, залить `.npk` в Files, перезагрузить. `/system/package/print`
   должен показать `container` включённым.
2. Разрешить контейнеры:

   ```
   /system/device-mode/update container=yes
   ```

   RouterOS попросит подтвердить физически: в течение 5 минут нажать кнопку reset
   или передёрнуть питание. Без этого контейнеры не запускаются.

## 3. Сеть контейнера

```
/interface/veth/add name=veth-eve address=172.17.0.2/24 gateway=172.17.0.1
/interface/bridge/add name=containers
/interface/bridge/port/add bridge=containers interface=veth-eve
/ip/address/add address=172.17.0.1/24 interface=containers
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24
```

Наружу (из LAN) — dst-nat на адрес роутера:

```
/ip/firewall/nat/add chain=dstnat dst-port=8080 protocol=tcp action=dst-nat \
    to-addresses=172.17.0.2 to-ports=8080 in-interface-list=LAN
```

## 4. Диск и файлы

`/disk print` покажет и устройство, и разделы. **Путь берётся от MOUNT-POINT раздела
(флаги `BMp`), а не от слота устройства**: у нас это `usb1-part1`, слот `usb1` —
несмонтированный блочный девайс, и путь через него не найдётся.

```
usb1-part1/eve/data     ← монтируется в /data: sde.db, eve-empire.db
usb1-part1/eve/root     ← распакованный образ (root-dir), ~40 МБ
```

**Файловая система — только ext4.** `/disk print detail` покажет `fs=`. На FAT32/exFAT
нет ни бита исполнения, ни симлинков, и распаковка слоя в `root-dir` либо падает, либо
даёт неработающий контейнер; плюс SQLite на FAT ведёт себя непредсказуемо с блокировками.
Форматирование стирает диск целиком:

```
/disk format-drive [find slot=usb1] file-system=ext4 label=eve
```

`sde.db` проще принести физически: вставить флешку в ПК, создать `eve/data`,
скопировать туда `sde.db`, вернуть в роутер. 250 МБ через WinBox Files тоже
загрузятся, но долго. ГРАБЛЯ: ext4 Windows штатно не читает — либо форматировать
диск на роутере и лить файл по FTP/WinBox, либо держать на диске ext4 и копировать
с Linux-машины.

## 5. Переменные окружения

Файл `.env` не нужен — всё через `/container/envs` (тогда рабочий каталог ни на что
не влияет). Пути обязательно абсолютные:

```
/container/envs/add list=eve key=EVE_CLIENT_ID     value=<id>
/container/envs/add list=eve key=EVE_CLIENT_SECRET value=<secret>
/container/envs/add list=eve key=ENCRYPTION_KEY    value=<64 hex>
/container/envs/add list=eve key=EVE_CALLBACK_URL  value=http://192.168.88.1:8080/callback
/container/envs/add list=eve key=DB_PATH           value=/data/eve-empire.db
/container/envs/add list=eve key=SDE_PATH          value=/data/sde.db
/container/envs/add list=eve key=ESI_USER_AGENT    value="eve-empire/0.1 (contact: you@example.com)"
/container/envs/add list=eve key=TZ                value=Asia/Novosibirsk
```

`ENCRYPTION_KEY` — **тот же самый**, иначе сохранённые refresh-токены не расшифруются
(перенос базы с ПК на роутер имеет смысл только с тем же ключом).

**Callback**: адрес в `EVE_CALLBACK_URL` должен совпадать с указанным в приложении на
developers.eveonline.com. Если портал разрешает только один URL, дев-запуск на ПК и
роутер придётся переключать там же (или завести второе приложение).

## 6. Импорт и запуск

Залить `dist\eve-empire-arm64.tar` в Files роутера, затем:

```
/container/mounts/add list=eve-data src=usb1-part1/eve/data dst=/data
/container/add file=eve-empire-arm64.tar interface=veth-eve root-dir=usb1-part1/eve/root \
    mountlists=eve-data envlists=eve workdir=/data entrypoint=/app/eve-empire \
    dns=8.8.8.8 logging=yes start-on-boot=yes name=eve-empire
/container/print          # дождаться status=stopped (распаковка занимает минуту)
/container/start [find name=eve-empire]
/log/print where topics~"container"
```

`file=` берёт наш тар как есть, распаковывать/жать не надо (RouterOS зовёт это
«tar.gz tarball», но `docker save` отдаёт обычный tar, и импортируется именно он).

В 7.23 появился выключатель `network-outgoing-access` — **он должен остаться
разрешённым**, весь ESI ходит наружу из контейнера.

После старта — `http://<ip роутера>:8080`.

## 7. Обновление

```powershell
.\deploy\build-arm64.ps1
```

на роутере: `/container/stop [find name=eve-empire]`, дождаться `status=stopped`,
`/container/remove [find name=eve-empire]`, залить новый тар, повторить `/container/add`
(mounts/envs/veth остаются). Каталог `usb1-part1/eve/data` не трогаем — базы переживают
пересборку.

## Если не поднялось

| симптом | почему |
|---|---|
| `/container/add` ругается на `mountlists`/`list` | версия старше 7.21 — там `mounts=` и `name=` |
| `/container/add` ругается на `envlists` | до 7.21 свойство звалось `envlist` (единственное); в 7.23 оба списка во множественном |
| контейнер добавился, но `status=error` | не хватило места под распаковку в `root-dir` либо архитектура не та (`file` внутри — arm64) |
| стартует и сразу глохнет, в логе `EVE_CLIENT_ID and EVE_CLIENT_SECRET must be set` | не подхватился `envlist=eve` (проверь `/container/print detail`) |
| в логе `ENCRYPTION_KEY must be 64 hex characters` | значение обрезалось при вводе — заводить в кавычках |
| страницы пустые, в логе TLS-ошибки | нет DNS или выхода наружу: `/container/print` → `dns`, srcnat masquerade, `network-outgoing-access` |
| «статическая база /data/sde.db не найдена» | не смонтировался `mountlists`, `sde.db` лежит не в `usb1-part1/eve/data` или путь указан через слот `usb1` вместо mount-point раздела |
| логин в EVE уводит на `localhost:8080` | в `EVE_CALLBACK_URL` остался дев-адрес |

Логи контейнера идут в общий лог роутера только при `logging=yes`:
`/log/print where topics~"container"`.

## Что помнить

- **Бэкап SQLite** — только при остановленном контейнере: база в WAL-режиме, копировать
  надо `eve-empire.db` вместе с `-wal` и `-shm`.
- Свободной памяти на RB5009 около 1 ГБ на всё; тяжёлые страницы (региональные ордера
  Житы — 93 МБ raw в 16 потоков) на роутере будут заметно медленнее и прожорливее, чем
  на ПК.
- Фоновых задач в приложении пока нет — данные собираются только при заходе на
  страницу, так что «сервер работает, история копится сама» это пока не про нас
  (см. TASKS.md).
- Порт 8080 на самом роутере не занят службами RouterOS, но проверь `/ip/service/print`,
  если менял порты www.
