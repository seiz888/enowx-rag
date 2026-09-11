<#
.SYNOPSIS
    Register the off-host ledger copy as a Scheduled Task.

.DESCRIPTION
    Creates \memgw\ledger-offhost, which runs push-ledger-offhost.ps1 daily and
    at logon.

    Why this task exists
    --------------------
    The sealed ledger backup lives on D: on the same workstation as the ledger.
    A separate volume is not a separate failure domain: a lost, stolen or
    wiped machine takes both copies, and the ledger is the authority for every
    agent's work state. This task puts a copy on the VPS, so losing the
    workstation no longer means losing the ledger.

    Why it runs after the ledger backup
    -----------------------------------
    It ships the newest sealed artefact, so it must run after \memgw\ledger-backup
    has produced one. 03:30 local gives the 02:30 ledger backup an hour of slack;
    StartWhenAvailable covers a machine that was off, and running with no new
    artefact simply re-verifies the one already there (it is idempotent).

    Interaction with \memgw\backup-pull
    -----------------------------------
    Different task, different direction, different artefact, own manifest. The
    pull brings VPS database dumps down; this push sends the sealed ledger up.
    Neither installer removes the other's task.

.PARAMETER SkipVerify
    Register the task without running the copy once to verify.

.PARAMETER WhatIf
    Report what would happen without creating anything.

.NOTES
    Read-only with respect to the ledger. No secret leaves the machine: the
    payload is already DPAPI-sealed and the key never travels.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$SkipVerify
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

$root = 'D:\memgw'
$taskPath = '\memgw\'
$taskName = 'ledger-offhost'
$fullName = "$taskPath$taskName"
$manifestPath = Join-Path $root 'run\installed-ledger-offhost.json'

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

$script = Join-Path $PSScriptRoot 'push-ledger-offhost.ps1'
if (-not (Test-Path -LiteralPath $script)) { throw "off-host copy script not found: $script" }

New-Item -ItemType Directory -Force -Path (Join-Path $root 'run') | Out-Null

$powershell = (Get-Command powershell.exe).Source
$action = New-ScheduledTaskAction `
    -Execute $powershell `
    -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $script) `
    -WorkingDirectory $root

$daily = New-ScheduledTaskTrigger -Daily -At '03:30'
$logon = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 30) `
    -RestartCount 2 `
    -RestartInterval (New-TimeSpan -Minutes 5)

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host "would register $fullName -> $script"
    exit 0
}

Register-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Action $action `
    -Trigger @($daily, $logon) -Settings $settings -Principal $principal `
    -Description 'memgw: copy the sealed ledger backup to the VPS (different failure domain), verify size+sha256+permissions remotely.' | Out-Null

$registered = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName
[ordered]@{
    task_path   = $taskPath
    task_name   = $taskName
    script      = $script
    action_hash = Get-TaskActionHash -Task $registered
    installed_at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ', [System.Globalization.CultureInfo]::InvariantCulture)
} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding utf8

Write-Host "registered: $fullName"
Write-Host "manifest  : $manifestPath"

if ($SkipVerify) { exit 0 }

Write-Host 'verifying: running the off-host copy once...'
$v = Start-Process -FilePath $powershell `
    -ArgumentList @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $script) `
    -PassThru -Wait -WindowStyle Hidden
Write-Host "verification exit: $($v.ExitCode)"
if ($v.ExitCode -ne 0) { throw "the off-host ledger copy failed (exit $($v.ExitCode))" }
Write-Host 'verification OK'
