# FOGs

Сервер развёртывания образов Windows для компьютерного клуба — замена FOG без
Linux-компонентов и с вменяемым интерфейсом. Рабочее название проекта — FOGs.

Из FOG берём идею «хост → задача → агент», блочное копирование только занятых
кластеров и multicast. Клиенты — Windows в обычном WinPE под UEFI, сервер —
один самодостаточный бинарник на Go.

## Статус

Проектирование завершено, начат код. Готов пакет [`proto`](proto) — формат файла
образа (заголовок, bitmap занятых кластеров, независимые сжатые чанки) с
golden-тестами. Сервер и агент ещё не написаны.

Все принятые решения, открытые вопросы и план работ — в
[docs/DESIGN.md](docs/DESIGN.md).

## Структура

| Путь | Назначение |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | проектные решения, открытые вопросы, план |
| [`proto/`](proto) | формат образа и протокол обмена (общий пакет для сервера и агента) |
| `server/` | HTTP API, веб-интерфейс, очередь задач, хранилище образов, multicast (план) |
| `agent/` | бинарник `windows/amd64` для WinPE (план) |
| `winpe/` | скрипты подготовки WinPE, iPXE, wimboot (план) |
| [`reference/`](reference) | исходники FOG, FOG OS и partclone как сабмодули (см. ниже) |

## Сборка и тесты

Тулчейн живёт в WSL-дистрибутиве `claude`, на хосте Windows ничего не ставим:

```bash
wsl -d claude -e bash -lc "cd /mnt/g/Wsl-space/FOGs && go vet ./... && go test ./..."
```

Кросс-компиляция агента под WinPE:

```bash
GOOS=windows GOARCH=amd64 go build -o bin/fogs-agent.exe ./agent
```

## Референсы

Исходники проектов, у которых заимствуются идеи, подключены как git-сабмодули в
`reference/`. Они нужны как материал для изучения устройства FOG и не входят в
сборку FOGs и не линкуются с нашим кодом.

| Путь | Проект | Upstream | Зачем |
|---|---|---|---|
| `reference/fogproject` | FOG | https://github.com/FOGProject/fogproject | серверная часть, PXE, веб-интерфейс |
| `reference/fos` | FOG OS | https://github.com/FOGProject/fos | bash-агент и мини-Linux на Buildroot |
| `reference/partclone` | partclone | https://github.com/Thomas-Tsai/partclone | формат образа и копирование занятых блоков |

Саблибы зафиксированы на конкретных коммитах, чтобы поведение ссылок не менялось
неожиданно. Лицензии — у исходных проектов, см. `LICENSE` в каждом каталоге.

Клонирование вместе с референсами:

```bash
git clone --recurse-submodules <url> FOGs
```

Если репозиторий уже склонирован без них:

```bash
git submodule update --init --recursive
```

Обновить референсы до свежих версий upstream (осознанно, с проверкой diff):

```bash
git submodule update --remote --merge
```

## Лицензия

Пока не выбрана.
