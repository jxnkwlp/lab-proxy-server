$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

$go = Get-Command go -ErrorAction SilentlyContinue
if ($null -eq $go) {
    $fallback = 'C:\Program Files\Go\bin\go.exe'
    if (-not (Test-Path $fallback)) {
        throw 'Go was not found in PATH or C:\Program Files\Go\bin\go.exe'
    }
    $goExe = $fallback
} else {
    $goExe = $go.Source
}

$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOMODCACHE = Join-Path $root '.modcache'
$env:GOPATH = Join-Path $root '.gopath'
$env:GOCACHE = Join-Path $root '.gocache'

$dist = Join-Path $root 'dist'
$exe = Join-Path $dist 'clash-server.exe'

New-Item -ItemType Directory -Force -Path $dist | Out-Null
& $goExe build -o $exe ./src
& $exe
