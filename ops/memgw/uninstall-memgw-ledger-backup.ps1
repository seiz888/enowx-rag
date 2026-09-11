<#
.SYNOPSIS
    Remove the memgw ledger backup Scheduled Task.

.DESCRIPTION
    Removes exactly \memgw\ledger-backup, and only if its action still matches
    the recorded hash. Mirrors the other uninstallers and keeps each task's
    lifecycle independent.

    It deliberately does not delete D:\memgw-backups: the sealed dumps are the
    ledger's only recovery point, and removing the task that makes them is a
    different decision from destroying the ones that exist.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

$manifestPath = 'D:\memgw\run\installed-ledger-backup.json'

function Get-TaskActionHash {
    param($Task)
    $a = $Task.Actions[0]
    $s = "$($a.Execute)|$($a.Arguments)|$($a.WorkingDirectory)"
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($s))) -replace '-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}

if (-not (Test-Path -LiteralPath $manifestPath)) {
    Write-Host "No manifest at $manifestPath -- nothing to remove."
    exit 0
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
Write-Host 'memgw ledger-backup uninstall'
Write-Host "  installed at : $($manifest.installed_at)"
Write-Host "  task         : $($manifest.task_path)$($manifest.task_name)"

$task = Get-ScheduledTask -TaskPath $manifest.task_path -TaskName $manifest.task_name -ErrorAction SilentlyContinue
if (-not $task) {
    Write-Host '  task is already absent.'
} else {
    $actual = Get-TaskActionHash -Task $task
    if ($actual -ne $manifest.action_hash) {
        Write-Host 'REFUSING: the task action does not match the install manifest.'
        throw 'the task was edited by hand; remove it yourself if that is intended'
    }
    Unregister-ScheduledTask -TaskPath $manifest.task_path -TaskName $manifest.task_name -Confirm:$false
    Write-Host "  removed: $($manifest.task_path)$($manifest.task_name)"
}
Write-Host 'note: sealed dumps in D:\memgw-backups were NOT deleted.'
Write-Host 'done.'
