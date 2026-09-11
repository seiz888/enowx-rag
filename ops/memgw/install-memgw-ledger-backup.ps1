<#
.SYNOPSIS
    Register the memgw ledger backup as a Scheduled Task.

.DESCRIPTION
    Creates \memgw\ledger-backup, which runs D:\memgw\backup.ps1 daily and at
    logon. This is the piece that was missing: backup.ps1 existed and worked,
    but nothing ever called it, so the canonical ledger (container memgw-live)
    had no recovery point at all -- the only artefact on disk was a 1,270-byte
    "dump" that was a PowerShell text-mode redirect of binary output and
    restored nothing.

    Daily, not hourly: the ledger is a write-ahead log of agent activity, the
    dump is a few hundred KB, and the value of a backup here is a recovery
    point, not a low RPO. Hourly would add nothing a replay from the last daily
    dump plus the gateway's own event log does not already cover.

    Why a separate task and manifest
    --------------------------------
    \memgw\supervisor keeps the stack alive and must never fail for a backup
    reason; \memgw\healthcheck probes and must never restart anything. Each has
    its own installer, manifest and uninstaller so no one removes another's
    task. This follows that pattern exactly.

    Why the task can fail visibly
    -----------------------------
    backup.ps1 exits non-zero on any failure (container down, wrong magic,
    too few TOC entries, sealed copy that does not unseal to the dump). Task
    Scheduler records that in "Last Run Result", so a silently empty backup --
    the exact failure this task exists to prevent -- shows up as a non-zero
    result without anyone reading a log.

.PARAMETER SkipVerify
    Register the task without running the backup once to verify.

.PARAMETER WhatIf
    Report what would happen without creating anything.

.NOTES
    Read-only with respect to the repository and to agent settings. Does not
    touch the protected seizrag-qdrant container or the sc_recovery_* volumes.
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
$taskName = 'ledger-backup'
$fullName = "$taskPath$taskName"
$manifestPath = Join-Path $root 'run\installed-ledger-backup.json'

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

$script = Join-Path $root 'backup.ps1'
if (-not (Test-Path -LiteralPath $script)) { throw "backup script not found: $script" }

New-Item -ItemType Directory -Force -Path (Join-Path $root 'run') | Out-Null

$powershell = (Get-Command powershell.exe).Source
$action = New-ScheduledTaskAction `
    -Execute $powershell `
    -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $script) `
    -WorkingDirectory $root

# 02:30 local, before the off-host pull at 11:00, so a sealed ledger copy is
# always available to whatever pulls next. StartWhenAvailable covers a machine
# that was off at the scheduled time.
$daily = New-ScheduledTaskTrigger -Daily -At '02:30'
$logon = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 15) `
    -RestartCount 2 `
    -RestartInterval (New-TimeSpan -Minutes 5)

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host "would register $fullName -> $script"
    exit 0
}

Register-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Action $action `
    -Trigger @($daily, $logon) -Settings $settings -Principal $principal `
    -Description 'memgw: sealed DPAPI backup of the canonical ledger (memgw-live) to D:\memgw-backups.' | Out-Null

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

Write-Host 'verifying: running the backup once...'
$v = Start-Process -FilePath $powershell `
    -ArgumentList @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $script) `
    -PassThru -Wait -WindowStyle Hidden
Write-Host "verification exit: $($v.ExitCode)"
if ($v.ExitCode -ne 0) { throw "the ledger backup failed (exit $($v.ExitCode))" }
Write-Host 'verification OK'
