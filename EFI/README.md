# Загрузочные файлы

Готовые файлы сторонних проектов, без изменений. Перед запуском сервера их нужно
скопировать в `-root`. `ipxe-shim.efi` и `ipxe.efi` должны быть из одной сборки.

| Файл | Откуда | Исходники |
|---|---|---|
| `ipxe-shim.efi` | `x86_64-sb/shimx64.efi` из `ipxeboot.tar.gz`, [релизы iPXE](https://github.com/ipxe/ipxe/releases) | [rhboot/shim](https://github.com/rhboot/shim) |
| `ipxe.efi` | `x86_64-sb/ipxe.efi` из того же архива | [ipxe/ipxe](https://github.com/ipxe/ipxe) |
| `wimboot` | [релизы wimboot](https://github.com/ipxe/wimboot/releases) | [ipxe/wimboot](https://github.com/ipxe/wimboot) |

Лицензии — у исходных проектов.
