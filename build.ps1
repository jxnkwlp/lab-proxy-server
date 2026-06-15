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
$env:CGO_ENABLED = '0'

$targets = @(
    @{ GOOS = 'windows'; GOARCH = 'amd64'; Ext = '.exe' },
    @{ GOOS = 'windows'; GOARCH = 'arm64'; Ext = '.exe' },
    @{ GOOS = 'linux'; GOARCH = 'amd64'; Ext = '' },
    @{ GOOS = 'linux'; GOARCH = 'arm64'; Ext = '' }
)

$dist = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null

foreach ($target in $targets) {
    $env:GOOS = $target.GOOS
    $env:GOARCH = $target.GOARCH

    $name = "$($target.GOOS)-$($target.GOARCH)"
    $outDir = Join-Path $dist $name
    $outFile = Join-Path $outDir "clash-server$($target.Ext)"

    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    & $goExe build -trimpath -ldflags "-s -w" -o $outFile ./src
    Write-Host "Built $outFile"
}
