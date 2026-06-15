# Clash Server

Clash Server is a small Go service that embeds [mihomo](https://github.com/MetaCubeX/mihomo) and exposes a local management UI. It stores service settings, downloads a subscription profile, renders the runtime mihomo profile, and proxies compatible controller requests to the embedded core.

## Features

- Local management UI on port `9090` by default.
- Configurable subscription URL, proxy ports, update interval, mode, log level, Geo database update interval, IPv6, and management port.
- Runtime config stored in `data/config.yml`.
- Downloaded and rendered mihomo profile stored in `data/profile.yml`.
- Immediate mihomo refresh after saving config.
- Manual subscription refresh from the UI or API.
- Subscription download retry with 2 attempts and a 10 second delay.
- Service-managed Geo database updates.
- Embedded mihomo controller access through the management port.
- Optional dashboard UI served from `src/static/dashboard` or `static/dashboard`.

## Requirements

- Go `1.26` or newer, as defined in `go.mod`.
- PowerShell for the helper scripts.

## Quick Start

Run the service directly:

```powershell
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOMODCACHE = "$PWD\.modcache"
$env:GOPATH = "$PWD\.gopath"
$env:GOCACHE = "$PWD\.gocache"
go run ./src
```

Then open:

```text
http://127.0.0.1:9090/ui/
```

On Windows, you can also build and run the local executable with:

```powershell
.\start.ps1
```

## Default Configuration

The service creates `data/config.yml` on first start when the file does not already exist.

| Setting                           | Default                       |
| --------------------------------- | ----------------------------- |
| Management UI and controller port | `9090`                        |
| Mixed proxy port                  | `7890`                        |
| HTTP proxy port                   | `7891`                        |
| SOCKS5 proxy port                 | `7892`                        |
| Subscription update interval      | `60` minutes                  |
| Mode                              | `rule`                        |
| Log level                         | `info`                        |
| Geo database update interval      | `24` hours                    |
| IPv6                              | `true`                        |
| Mihomo controller socket          | `data/mihomo-controller.sock` |

The rendered runtime profile is normalized with these service-managed values:

- `allow-lan: true`
- `geo-auto-update: false`
- `geo-update-interval` from the service config
- `sniffer.enable: true`
- `sniffer.override-destination: true`
- TLS SNI sniffing enabled

## Routes

| Path                                   | Description                                           |
| -------------------------------------- | ----------------------------------------------------- |
| `/ui/`                                 | Management UI                                         |
| `/ui/dashboard/`                       | Dashboard UI, when dashboard static files are present |
| `GET /api/admin`                       | Read service config and runtime status                |
| `POST /api/admin/config`               | Save service config and refresh mihomo                |
| `GET /api/admin/proxy-groups`          | Read proxy groups from the rendered profile           |
| `POST /api/admin/subscription/refresh` | Refresh the subscription immediately                  |
| `POST /api/admin/core/start`           | Start the embedded mihomo core                        |
| `POST /api/admin/core/stop`            | Stop the embedded mihomo core                         |
| `POST /api/admin/core/restart`         | Restart the embedded mihomo core                      |

Other paths, such as `/proxies`, `/configs`, and `/traffic`, are forwarded to the embedded mihomo controller.

## Dashboard Assets

The Docker build downloads the Yacd-meta dashboard automatically. For a local source build, place dashboard files under one of these directories if you want `/ui/dashboard/` to work:

- `src/static/dashboard`
- `static/dashboard`

Dashboard archive:

```text
https://codeload.github.com/MetaCubeX/Yacd-meta/zip/refs/heads/gh-pages
```

## Build

Build the Windows executable:

```powershell
.\build.ps
```

Create Windows and Linux release artifacts for `amd64` and `arm64`:

```powershell
.\build.ps1
```

Manual build:

```powershell
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOMODCACHE = "$PWD\.modcache"
$env:GOPATH = "$PWD\.gopath"
$env:GOCACHE = "$PWD\.gocache"
go build -o .\dist\clash-server.exe ./src
```

## Test

```powershell
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOMODCACHE = "$PWD\.modcache"
$env:GOPATH = "$PWD\.gopath"
$env:GOCACHE = "$PWD\.gocache"
go test ./...
```

## Docker

Build the image:

```powershell
docker build -t clash-server .
```

Run it with a persistent data directory:

```powershell
docker run --rm -p 9090:9090 -p 7890:7890 -p 7891:7891 -p 7892:7892 -v ${PWD}\data:/app/data clash-server
```
