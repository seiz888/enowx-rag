<#
.SYNOPSIS
    Register the off-host pull of the memgw ledger as a Scheduled Task.

.DESCRIPTION
    Creates \memgw\ledger-pull, which runs pull-memgw-backup.ps1 daily and at
    logon.

    Why this task exists
    --------------------
    After the migration the VPS is authoritative for the ledger and this
    workstation holds the off-host copy. That direction was documented and
    pull-memgw-backup.ps1 existed -- but NOTHING SCHEDULED IT. The only tasks
    that ran were the two now-retired pushers, which shipped artefacts of the
    retired local ledger: exactly the shape of a backup pipeline that looks
    alive while producing copies of the wrong database.

    A pull is the right direction for an off-host copy. It survives a
    compromised or lost source host, which a push cannot: a push has to be
    trusted by the machine that holds the data, and it runs there.

    Why it runs after the VPS takes its backup
    ------------------------------------------
    The VPS backup timer produces the artefact; this task fetches it. 04:30
    local gives the 03:30 window a clear hour, and StartWhenAvailable covers a
    machine that was off. The pull is idempotent: an artefact already sealed
    locally is not fetched again.

    Relationship to the retired tasks
    ---------------------------------
    \memgw\ledger-backup and \memgw\ledger-offhost are retired. The first
    produced a backup FROM the local container, which no longer holds the
    ledger; the second pushed that artefact to the VPS, so both could only ever
    move stale data. Their replacements are this task (fetch the authoritative
    artefact) and the VPS's own backup timer (produce it).

.PARAMETER SkipVerify
    Register the task without pulling once to verify.

.PARAMETER WhatIf
    Report what would happen without creating anything.

.NOTES
    Read-only with respect to the ledger. The recovery proof unseals a fetched
    artefact with the ESCROWED key to prove it is recoverable, and the key is
    fetched to a temporary 0600 file that is removed afterwards. No secret is
    written to the repository or to any log.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$SkipVerify
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

. (Join-Path $PSScriptRoot 'Memgw.Common.ps1')

$root = 'D:\memgw'
$taskPath = '\memgw\'
$taskName = 'ledger-pull'
$fullName = "$taskPath$taskName"
$manifestPath = Join-Path $root 'run\installed-ledger-pull.json'

function Get-TaskActionHash {
    param($Task)
    $a = $Task.Actions[0]
    $s = "$($a.Execute)|$($a.Arguments)|$($a.WorkingDirectory)"
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($s))) -replace '-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}

if (Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName -ErrorAction SilentlyContinue) {
    throw "task $fullName already exists. Remove it deliberately first, or run the uninstall."
}

$script = Join-Path $PSScriptRoot 'pull-memgw-backup.ps1'
if (-not (Test-Path -LiteralPath $script)) { throw "ledger pull script not found: $script" }

New-Item -ItemType Directory -Force -Path (Join-Path $root 'run') | Out-Null

# Windowless action; see New-MemgwHiddenScriptAction.
$action = New-MemgwHiddenScriptAction -ScriptPath $script

$daily = New-ScheduledTaskTrigger -Daily -At '04:30'
$logon = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# The pull transfers an artefact and then unseals it to prove recoverability, so
# it is given more headroom than the pushers had. Still bounded: an unbounded
# task is one that can wedge and hold the pipe silently.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 45) `
    -RestartCount 2 `
    -RestartInterval (New-TimeSpan -Minutes 5)

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host 'WhatIf: would register'
    Write-Host "  task    : $fullName"
    Write-Host "  execute : $($action.Execute)"
    Write-Host "  args    : $($action.Arguments)"
    Write-Host '  trigger : daily 04:30 and at logon'
    Write-Host ''
    Write-Host 'No changes made.'
    exit 0
}

Register-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Action $action `
    -Trigger @($daily, $logon) -Settings $settings -Principal $principal `
    -Description 'memgw: fetch the authoritative ledger backup from the VPS into the off-host copy, verify sha256, and prove recovery with the escrowed key.' | Out-Null

$registered = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName
[ordered]@{
    installed_at = (Get-Date).ToString('o')
    installed_by = "$env:USERDOMAIN\$env:USERNAME"
    task_path    = $taskPath
    task_name    = $taskName
    action_hash  = Get-TaskActionHash -Task $registered
    script       = $script
    note         = 'Remove with uninstall-memgw-ledger-pull.ps1. Do not edit by hand; the hash will fail.'
} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding utf8

Write-Host "registered: $fullName"
Write-Host "manifest  : $manifestPath"

if ($SkipVerify) { exit 0 }

Write-Host 'verifying: running the ledger pull once...'
$verifyExit = Start-MemgwHiddenScript -ScriptPath $script
Write-Host "verification exit: $verifyExit"
if ($verifyExit -ne 0) { throw "the ledger pull failed (exit $verifyExit)" }
Write-Host 'verification OK: the authoritative artefact was fetched and proved recoverable'
