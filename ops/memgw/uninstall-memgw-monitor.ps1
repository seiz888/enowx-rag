<#
.SYNOPSIS
    Remove the memgw health-check Scheduled Task.

.DESCRIPTION
    Removes exactly the task recorded in D:\memgw\run\installed-monitor.json,
    and only if its action still matches the recorded hash. If the manifest is
    absent, nothing is removed and the script says so; if the action was edited
    by hand, the script refuses rather than deleting something it did not
    create. This mirrors uninstall-memgw-persistence.ps1 and keeps the two
    tasks independent.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:MemgwRoot = 'D:\memgw'
$manifestPath = Join-Path $script:MemgwRoot 'run\installed-monitor.json'

function Get-MemgwTaskActionHash {
    param($Task)
    $a = $Task.Actions[0]
    $s = "$($a.Execute)|$($a.Arguments)|$($a.WorkingDirectory)"
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($s)
        return ([BitConverter]::ToString($sha.ComputeHash($bytes)) -replace '-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}

if (-not (Test-Path -LiteralPath $manifestPath)) {
    Write-Host "No monitor manifest at $manifestPath -- nothing to remove."
    exit 0
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
$taskPath = $manifest.task_path
$taskName = $manifest.task_name

Write-Host 'memgw monitor uninstall'
Write-Host "  installed at : $($manifest.installed_at)"
Write-Host "  task         : $taskPath$taskName"

$task = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName -ErrorAction SilentlyContinue
if (-not $task) {
    Write-Host '  task is already absent.'
} else {
    $actualHash = Get-MemgwTaskActionHash -Task $task
    if ($actualHash -ne $manifest.action_hash) {
        Write-Host 'REFUSING: the task action does not match the install manifest.'
        Write-Host "  recorded : $($manifest.action_hash)"
        Write-Host "  actual   : $actualHash"
        throw 'the task was edited by hand; remove it yourself if that is intended'
    }
    Unregister-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Confirm:$false
    Write-Host "  removed: $taskPath$taskName"
}

Write-Host 'done.'
