<#
.SYNOPSIS
    Remove the off-host ledger copy Scheduled Task.

.DESCRIPTION
    Removes exactly \memgw\ledger-offhost, and only if its action still matches
    the recorded hash. Mirrors the other uninstallers.

    It deliberately does not delete the artefact on the VPS. That copy is the
    ledger's last line of defence against losing this workstation; removing the
    task that keeps it fresh is a different decision from destroying it, and the
    remote path is root-only so this script could not reach it unprivileged
    anyway.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

$manifestPath = 'D:\memgw\run\installed-ledger-offhost.json'

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
Write-Host 'memgw ledger-offhost uninstall'
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
Write-Host 'note: the VPS copy at /opt/rag-backup/memgw-ledger-offhost was NOT deleted.'
Write-Host 'done.'
