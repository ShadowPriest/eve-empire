# EVE Empire: установка (Debian / Ubuntu)

Локальный веб-кабинет для альтов EVE Online. Один статический бинарник и
SQLite, никаких внешних зависимостей. Ставится как служба systemd под
пользователем `eve-empire`.

```
sudo apt install ./eve-empire_<версия>_amd64.deb
```

Что появится:

| путь | что |
|---|---|
| `/usr/bin/eve-empire` | сервер |
| `/usr/bin/eve-sdeimport` | импорт статической базы SDE |
| `/etc/eve-empire/eve-empire.env` | настройки и секреты (не затирается при обновлении) |
| `/var/lib/eve-empire/` | базы: `eve-empire.db` (токены) и `sde.db` |
| `/lib/systemd/system/eve-empire.service` | служба |

## 1. Зарегистрируй приложение EVE SSO

На [developers.eveonline.com](https://developers.eveonline.com) создай
приложение с Callback URL `http://<адрес-машины>:8080/callback` (по умолчанию
`http://localhost:8080/callback`). Scopes: отметь все, сервер сам запросит
только нужные. Получишь Client ID и Secret Key.

## 2. Настрой /etc/eve-empire/eve-empire.env

Впиши `EVE_CLIENT_ID`, `EVE_CLIENT_SECRET`, при необходимости
`EVE_CALLBACK_URL`, и сгенерируй ключ:

```
openssl rand -hex 32
```

Результат впиши в `ENCRYPTION_KEY`. Им шифруются refresh-токены персонажей.

## 3. Статическая база (рекомендуется)

Состав руд, чертежи, атрибуты и иконки читаются из локальной копии
официального Static Data Export (~250 МБ). Собирается один раз от имени
пользователя службы, чтобы права на файл были правильные:

```
sudo -u eve-empire eve-sdeimport -db /var/lib/eve-empire/sde.db
```

Импорт качает данные с серверов CCP и может занять заметное время;
повторный запуск докачивает недостающее. Без `sde.db` кабинет работает,
но часть страниц будет неполной.

## 4. Запуск

```
sudo systemctl start eve-empire
sudo systemctl status eve-empire
journalctl -u eve-empire -f
```

Открой `http://<адрес-машины>:8080` и добавь персонажей через логин EVE SSO.
Служба включена в автозагрузку при установке; после правки настроек
`systemctl restart eve-empire`.

Токены персонажей лежат в `/var/lib/eve-empire/eve-empire.db`, поэтому
запускать сервис стоит только дома (LAN/VPN), не на публичном хостинге.

## Обновление и удаление

Обновление: тот же `apt install ./eve-empire_<новая>_amd64.deb`. Настройки
и базы остаются, служба перезапустится сама, если работала.

`apt remove eve-empire` оставляет базы и настройки. `apt purge eve-empire`
удаляет всё, включая токены и ключ; после этого альтов придётся логинить
заново.

## Лицензия

GNU AGPL-3.0. Исходники: https://github.com/ShadowPriest/eve-empire.
EVE Online и связанные материалы: собственность Fenris Creations.
