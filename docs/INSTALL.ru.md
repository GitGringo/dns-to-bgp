# Руководство по установке и настройке

[English version](INSTALL.md)

Руководство проведёт от Linux-сервера с уже работающими Unbound и BIRD до рабочей связки DNS-to-BGP. Что делает проект, описано в [README](../README.ru.md).

Все адреса и номера AS здесь — placeholder'ы для документации (`192.0.2.0/24`, `2001:db8::/32`, приватная AS `64512`). Замените их своими.

## 1. Требования

| Компонент | Требование |
|---|---|
| ОС | Linux с systemd (проверено на Ubuntu 24.04) |
| Unbound | Собран с Python-модулем. Проверка: `unbound -V` должен показывать `python` в «Linked modules» (пакеты Debian/Ubuntu собраны так). |
| BIRD | 2.x — BGP-спикер, принимающий маршруты |
| Go | 1.24+ для сборки демона (Go 1.21+ сам скачает нужный toolchain) |
| conntrack | Только для необязательного keepalive по трафику: `apt install conntrack` |

Python-модуль работает внутри процесса `unbound`. Демон запускается от того же пользователя (`unbound`), чтобы оба могли пользоваться одним UNIX-сокетом.

## 2. Получить исходники и собрать

```bash
git clone https://github.com/GitGringo/dns-to-bgp.git
cd dns-to-bgp
make build            # создаст bin/unbound-route-state
```

`make build` вызывает `go build` с `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` (небольшой статический бинарник). Без `make`:

```bash
cd unbound-route-state
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../bin/unbound-route-state ./cmd/unbound-route-state
```

## 3. Установить файлы

Ожидаемая раскладка (все пути — значения по умолчанию):

```
/opt/dns-to-bgp/
    unbound-bgp-announcer/      Python-модуль (грузится Unbound)
    unbound-route-state/        бинарник демона
    conf.d/                     announcer.conf, state.conf, communities.conf
    domains.d/                  файлы *.list с паттернами доменов
/run/dns-to-bgp/                сокеты (создаёт systemd)
```

Либо `sudo make install` (копирует всё, создаёт каталоги, существующие конфиги не перезаписывает), либо вручную:

```bash
sudo install -d /opt/dns-to-bgp/{unbound-bgp-announcer,unbound-route-state,conf.d,domains.d}
sudo install -m 0644 unbound-bgp-announcer/*.py /opt/dns-to-bgp/unbound-bgp-announcer/
sudo install -m 0755 bin/unbound-route-state    /opt/dns-to-bgp/unbound-route-state/
sudo install -m 0640 examples/conf.d/*.conf     /opt/dns-to-bgp/conf.d/
sudo install -m 0644 examples/domains.d/*.list  /opt/dns-to-bgp/domains.d/
sudo install -m 0644 examples/systemd/unbound-route-state.service /etc/systemd/system/
```

Затем права. Конфиги и каталог доменов должны читаться пользователем `unbound`:

```bash
sudo chown root:unbound /opt/dns-to-bgp/conf.d/*.conf /opt/dns-to-bgp/domains.d /opt/dns-to-bgp/domains.d/*.list
sudo chmod 0750 /opt/dns-to-bgp/conf.d && sudo chown root:unbound /opt/dns-to-bgp/conf.d
```

## 4. Настроить демон: `conf.d/state.conf`

Отредактируйте секцию `[general]`; остальное сначала можно оставить по умолчанию.

| Ключ | Что указать |
|---|---|
| `asn` | Ваш локальный номер AS. |
| `router_id` | Уникальный BGP router ID (значение в виде IPv4). |
| `local_address` | Адрес, который демон использует для BGP-сессии. |
| `neighbor`, `neighbor_asn` | Адрес и AS BIRD. |
| `passive` | `yes` (как в примере): демон слушает TCP-порт 1790, BIRD подключается к нему. |
| `next_hop` | Next hop по умолчанию для IPv4-маршрутов; пусто = `local_address`. Обычно — шлюз, через который должен идти трафик к этим хостам. |
| `next_hop_v6` | Next hop по умолчанию для IPv6-маршрутов. **Обязателен для любого IPv6**: если пуст, IPv6-маршруты не анонсируются. |

Каждый ключ описан в [docs/reference-route-state.md](reference-route-state.md) (на английском).

## 5. Выбрать домены

Списки доменов лежат в `/opt/dns-to-bgp/domains.d/*.list`, по одному паттерну на строку:

```
*.cdn.example.net     # только поддомены, сам cdn.example.net — нет
example.org           # только сам домен, без поддоменов
```

Анонсер сообщает только о DNS-ответах, подходящих под паттерны из этих файлов. Писать в каталог может что угодно (cron, загрузчик списков); файлы перечитываются каждые 5 секунд без рестарта Unbound. Пишите атомарно: создайте временный файл без расширения `.list` и переместите его (`mv`) на место.

## 6. Выбрать атрибуты маршрутов: `conf.d/communities.conf`

Файл определяет, какие BGP-атрибуты (community, next hop, local-pref, ...) получит маршрут в зависимости от домена. В заголовке блока перечисляются паттерны и/или файлы `NAME.list` из `domains.d`:

```ini
[corp.list, *.extra.example.com]
community = 64512:200

[default:missed]
community = 64512:999
```

По community в фильтрах BIRD (или на следующих маршрутизаторах) решайте, что делать с этими маршрутами. Все ключи и подводные камни (главный: никогда не указывать собственный ASN BIRD в `as_path`) — в [справочнике](reference-route-state.md#rules-communitiesconf). Файл перечитывается на лету.

## 7. Настройка анонсера: `conf.d/announcer.conf`

Значения по умолчанию уже соответствуют путям из этого руководства. `route_state_socket` меняйте, только если изменили `listen_socket` в `state.conf`.

## 8. Подключить модуль к Unbound

Скопируйте `examples/unbound/dns-to-bgp.conf` в `/etc/unbound/unbound.conf.d/` (или добавьте в `unbound.conf`):

```
server:
    module-config: "validator python iterator"
    chroot: ""
    prefetch: yes

python:
    python-script: "/opt/dns-to-bgp/unbound-bgp-announcer/announcer.py"
```

`chroot: ""` обязателен: Python-модуль не работает в chroot. Если дистрибутив ограничивает Unbound через AppArmor или SELinux, разрешите чтение `/opt/dns-to-bgp/` и запись в `/run/dns-to-bgp/state.sock`. В Ubuntu добавьте в `/etc/apparmor.d/local/usr.sbin.unbound`:

```
/opt/dns-to-bgp/** r,
/run/dns-to-bgp/state.sock rw,
```

и выполните `sudo apparmor_parser -r /etc/apparmor.d/usr.sbin.unbound`. Отказы видны в `dmesg | grep -i denied`.

Проверьте конфигурацию, но Unbound пока не перезапускайте: `sudo unbound-checkconf`.

## 9. Настроить BIRD

`examples/bird/dns-bgp.conf` — отправная точка: BGP-протокол, подключающийся к демону на порт 1790 и импортирующий хост-маршруты (`/32` и `/128`). Подключите его в конфигурацию BIRD, поправьте адреса и фильтры, затем:

```bash
sudo birdc configure
```

Демон анонсирует только маршруты `/32` и `/128`. BIRD должен принимать сессию как обычный BGP-пир; в примере iBGP (одинаковая AS с обеих сторон). Если адреса не в непосредственно подключённой подсети, раскомментируйте `multihop`.

## 10. Запуск

Сначала демон, затем Unbound:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now unbound-route-state
sudo systemctl restart unbound
sudo journalctl -u unbound-route-state -n 20 --no-pager
```

В логе ожидайте `listening on /run/dns-to-bgp/state.sock`. Подключился ли BIRD, показывает `ctl bgp` (см. ниже). Перезапуск Unbound нужен только при первой установке модуля или изменении его кода; списки доменов и правила перечитываются на лету.

## 11. Проверка

```bash
CTL="sudo /opt/dns-to-bgp/unbound-route-state/unbound-route-state ctl"

$CTL bgp                            # состояние сессии с BIRD: Established
dig @127.0.0.1 www.cdn.example.net  # резолвим имя, подходящее под паттерн
$CTL routes                         # адреса из ответа стали живыми маршрутами
$CTL route 203.0.113.10             # все атрибуты одного маршрута
$CTL stats                          # счётчики
sudo birdc show route protocol dns_bgp_daemon
```

Когда DNS TTL истекает, маршрут снимается автоматически (если не включён keepalive, см. ниже).

## 12. Дополнительные возможности

Обе выключены по умолчанию и описаны в [справочнике](reference-route-state.md).

- **Держать маршрут, пока идёт трафик** (`[keepalive] enabled = yes`): через `conntrack -L` маршрут остаётся анонсированным после истечения DNS TTL, пока к этому IP есть открытые соединения. Нужен пакет `conntrack`. Приложенный systemd-unit уже выдаёт `CAP_NET_ADMIN`.
- **Сброс кэша Unbound при старте** (`[unbound] flush_on_start = yes`, а также `ctl flush` в любой момент): Unbound отвечает на повторные запросы из кэша, не вызывая модуль, поэтому после рестарта демона закэшированные имена не появятся в маршрутах до истечения TTL. Сброс заставляет следующие запросы клиентов снова пройти через модуль. Нужен включённый `remote-control` в Unbound (есть в примере).

## 13. Обновление

```bash
git pull && make build && sudo make install
sudo systemctl restart unbound-route-state
sudo systemctl restart unbound          # только если менялись Python-файлы
```

После изменения Python-файлов делайте полный `systemctl restart unbound`. `unbound-control reload` может оставить в памяти старые копии вспомогательных модулей; при обнаружении этого модуль пишет в лог `RUNNING OLD CODE ... Restart Unbound`.

## 14. Диагностика

| Симптом | Причина и решение |
|---|---|
| Unbound пишет `FAILED TO START ... next retry in ...` | Модуль не смог прочитать конфиг или каталог доменов. Устраните причину из сообщения (путь, права, AppArmor); он повторяет попытки сам: раз в 60 с, затем раз в час. |
| `dropped N event(s) ... Broken pipe` / маршруты не появляются | Демон не запущен, либо пути сокета в `announcer.conf` и `state.conf` различаются. |
| Маршруты анонсируются, но BIRD их не показывает | `as_path` в `communities.conf` содержит ASN самого BIRD (BIRD молча отбрасывает маршрут; демон пишет WARN), либо import-фильтр BIRD их отклоняет. |
| Нет IPv6-маршрутов | В `state.conf` пуст `next_hop_v6`. |
| Предупреждение `events channel full` | Увеличьте `event_queue_size` в `[limits]`. |
| BGP-сессия поднимается минутами после рестарта | Задержка на стороне BIRD (`error wait time`); см. «BGP session recovery after a restart» в [справочнике](reference-route-state.md). |
| После рестарта демона маршруты пропали и возвращаются медленно | Ожидаемо: таблица стартует пустой. Включите graceful restart в BIRD и `flush_on_start`. |
| `ctl` отвечает permission denied | Запускайте через `sudo` или добавьте пользователя в группу `unbound`. |
