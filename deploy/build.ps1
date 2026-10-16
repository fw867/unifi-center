# Build script for the fnOS package (.fpk) - Windows development host.
# Produces a self-contained package: the service is a statically linked Go binary,
# so the target device needs no Python/Node/runtime app at all.
#
# Output (default: both architectures):
#     dist\unificenter-<version>-x86.fpk    platform=x86   linux/amd64
#     dist\unificenter-<version>-arm.fpk    platform=arm   linux/arm64
#
# Packing itself is done by tools\pack_fpk.py (Python stdlib) instead of tar: Windows
# bsdtar does not support --mode and records 0666, which would leave bin/unifi-center
# and cmd/* non-executable inside the package (Permission denied on the device).
#
# NOTE: this file is pure ASCII so that Windows PowerShell 5.1 parses it correctly
# regardless of the system code page.
#
# Usage:
#     powershell -File deploy\build.ps1
#     powershell -File deploy\build.ps1 -Arches x86
#     powershell -File deploy\build.ps1 -Arches arm -OutDir D:\out
param(
  [string]$Root = (Join-Path $PSScriptRoot '..'),
  [string]$Arches = 'x86,arm',
  [string]$OutDir = '',
  [string]$Python = 'python'
)

$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path $Root).Path
if (-not $OutDir) { $OutDir = Join-Path $Root 'dist' }

function Say([string]$Message) { Write-Host "[build] $Message" }
function Fail([string]$Message) {
  Write-Host "[build] ERROR: $Message" -ForegroundColor Red
  exit 1
}

# manifest must stay UTF-8 without BOM (fnpack reads appname from the first line).
function Read-Text([string]$Path) {
  return [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::UTF8).TrimStart([char]0xFEFF)
}

# --- pre-flight ------------------------------------------------------------
$manifestPath = Join-Path $Root 'manifest'
if (-not (Test-Path $manifestPath)) { Fail 'missing manifest' }
$manifest = @{}
(Read-Text $manifestPath) -split "`n" | ForEach-Object {
  if ($_ -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') { $manifest[$Matches[1]] = $Matches[2].Trim() }
}
$AppName = $manifest['appname']
$Version = $manifest['version']
if (-not $AppName) { Fail 'manifest has no appname' }
if (-not $Version) { Fail 'manifest has no version' }

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) { Fail 'go not found (needed to cross-compile the static binary)' }
$pythonCmd = Get-Command $Python -ErrorAction SilentlyContinue
if (-not $pythonCmd) { Fail "python not found (needed by tools\pack_fpk.py); pass -Python <path>" }
$packer = Join-Path $Root 'tools\pack_fpk.py'
if (-not (Test-Path $packer)) { Fail 'missing tools\pack_fpk.py' }

$required = @(
  'config\privilege', 'config\resource', 'cmd\main',
  'ICON.PNG', 'ICON_256.PNG', 'app\ui\config', 'app\ui\panel.html', 'app\ui\panel.js',
  'app\ui\panel.css', 'app\ui\assets\vue.js', 'app\ui\assets\lucide.js',
  'app\ui\assets\tailwind.js', 'wizard\install', 'wizard\upgrade', 'wizard\uninstall',
  'wizard\config', 'service\go.mod', 'service\main.go',
  'tools\make_icon.py', 'tools\ui-apple-touch-icon.png'
)
foreach ($item in $required) {
  if (-not (Test-Path (Join-Path $Root $item))) { Fail "missing $item" }
}
foreach ($json in @('config\privilege', 'config\resource', 'app\ui\config',
                    'wizard\install', 'wizard\upgrade', 'wizard\uninstall', 'wizard\config')) {
  $path = Join-Path $Root $json
  try { (Read-Text $path) | ConvertFrom-Json | Out-Null }
  catch { Fail "$json is not valid JSON: $($_.Exception.Message)" }
}

Get-ChildItem -Path $Root -Recurse -Force -Include '.DS_Store' -ErrorAction SilentlyContinue |
  Remove-Item -Force -ErrorAction SilentlyContinue
Get-ChildItem -Path $Root -Recurse -Force -Directory -Filter '__pycache__' -ErrorAction SilentlyContinue |
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
$dev = Join-Path $Root '.dev'
if (Test-Path $dev) { Remove-Item -Recurse -Force $dev }

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$binDir = Join-Path $Root 'app\bin'
New-Item -ItemType Directory -Force -Path $binDir | Out-Null

# --- icons -----------------------------------------------------------------
# Regenerate the icons from the official ui.com artwork so a build never ships
# stale icons. tools\make_icon.py is pure stdlib (no Pillow/ImageMagick needed).
Say 'regenerating icons from tools\ui-apple-touch-icon.png'
& $pythonCmd.Source (Join-Path $Root 'tools\make_icon.py') $Root
if ($LASTEXITCODE -ne 0) { Fail 'icon generation failed' }

# --- per-architecture build -----------------------------------------------
function Build-Arch([string]$arch) {
  switch ($arch) {
    'x86' { $goarch = 'amd64' }
    'arm' { $goarch = 'arm64' }
    default { Fail "unsupported arch '$arch' (use x86 or arm)" }
  }

  Say "compiling linux/$goarch (CGO disabled, static)"
  $binary = Join-Path $binDir 'unifi-center'
  if (Test-Path $binary) { Remove-Item $binary -Force }
  $env:CGO_ENABLED = '0'
  $env:GOOS = 'linux'
  $env:GOARCH = $goarch
  Push-Location (Join-Path $Root 'service')
  try {
    & $go.Source build -trimpath -ldflags '-s -w' -o $binary .
    if ($LASTEXITCODE -ne 0) { Fail "go build failed for $goarch" }
  } finally { Pop-Location }
  Remove-Item Env:\CGO_ENABLED, Env:\GOOS, Env:\GOARCH -ErrorAction SilentlyContinue

  $sizeKB = [math]::Round((Get-Item $binary).Length / 1KB, 1)
  Say "binary size: $sizeKB KB"

  $fpk = Join-Path $OutDir "$AppName-$Version-$arch.fpk"
  & $pythonCmd.Source $packer --root $Root --out $fpk --arch $arch
  if ($LASTEXITCODE -ne 0) { Fail "packing failed for $arch" }

  $fpkKB = [math]::Round((Get-Item $fpk).Length / 1KB, 1)
  Say "built $fpk ($fpkKB KB)"
}

foreach ($arch in ($Arches -split ',')) {
  $name = $arch.Trim()
  if ($name) { Build-Arch $name }
}

Say 'install with: appcenter-cli install-fpk <the .fpk matching your CPU architecture>'
Say 'note: x86 packages only install on x86 devices, arm packages only on ARM devices.'
