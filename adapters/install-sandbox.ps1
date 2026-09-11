# Install the memgw lifecycle fragments into an isolated home.
#
# This script exists so the adapters can be exercised end to end without
# touching any of the six agent installations that are actually in use. It
# writes into the directory you name and nowhere else. It refuses to write into
# a real agent home, and it never edits a global configuration, a PATH entry, a
# node_modules tree, or an installed binary.
#
#   .\install-sandbox.ps1 -HomeDir D:\memgw-test\adapter-home -Config D:\memgw-test\adapter.json
#
# Undo is deleting the directory. There is nothing else to undo.

[CmdletBinding()]
param(
    # Where the fragments go. Created if missing; must not be a real agent home.
    [Parameter(Mandatory = $true)][string]$HomeDir,
    # The adapter configuration the fragments will point at. Must already exist:
    # this script does not write configuration, because a configuration written
    # by a script is one nobody read.
    [Parameter(Mandatory = $true)][string]$Config,
    # Which hosts to lay down. Default: all six.
    [string[]]$Hosts = @('omp', 'opencode', 'claude', 'codex', 'droid', 'hermes')
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path

# The refusal list. These are the directories the six live hosts read, and a
# sandbox install that landed in one of them would change the behaviour of a
# session somebody is in the middle of.
$forbidden = @(
    (Join-Path $env:USERPROFILE '.claude'),
    (Join-Path $env:USERPROFILE '.codex'),
    (Join-Path $env:USERPROFILE '.omp'),
    (Join-Path $env:USERPROFILE '.factory'),
    (Join-Path $env:USERPROFILE '.config\opencode'),
    (Join-Path $env:USERPROFILE '.bun\install\global')
)

$full = [System.IO.Path]::GetFullPath($HomeDir)
foreach ($f in $forbidden) {
    $ff = [System.IO.Path]::GetFullPath($f)
    if ($full -eq $ff -or $full.StartsWith($ff + [System.IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "refusing to install into $full: that is a live agent home. Name a disposable directory."
    }
}

if (-not (Test-Path -LiteralPath $Config)) {
    throw "the adapter configuration $Config does not exist; create it from adapter.example.json first"
}
$configFull = [System.IO.Path]::GetFullPath($Config)

New-Item -ItemType Directory -Force -Path $full | Out-Null

foreach ($h in $Hosts) {
    $dest = Join-Path $full $h
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    switch ($h) {
        'omp'      { Copy-Item (Join-Path $root 'omp\memgw-hooks.ts') $dest -Force }
        'opencode' { Copy-Item (Join-Path $root 'opencode\memgw-hooks.ts') $dest -Force }
        'claude'   { Copy-Item (Join-Path $root 'claude\settings.hooks.json') (Join-Path $dest 'settings.json') -Force }
        'droid'    { Copy-Item (Join-Path $root 'droid\settings.hooks.json') (Join-Path $dest 'settings.json') -Force }
        'codex'    { Copy-Item (Join-Path $root 'codex\hooks.json') $dest -Force }
        'hermes'   { Copy-Item (Join-Path $root 'hermes\memgw-hook.sh') $dest -Force }
    }
    Write-Host "installed $h -> $dest"
}

Write-Host ''
Write-Host 'Nothing global was changed. To use this sandbox, set:'
Write-Host "  `$env:MEMGW_ADAPTER_CONFIG = '$configFull'"
Write-Host "and point the host at $full."
Write-Host "To undo: Remove-Item -Recurse -Force '$full'"
