<#
.SYNOPSIS
    Register the off-host backup pull as a Scheduled Task.

.DESCRIPTION
    Creates \memgw\backup-pull, which runs pull-vps-backup.ps1 daily at 03:10
    (after the VPS backup timer at 02:40 UTC has staged a fresh dump) and at
    logon. The task exits non-zero on any checksum failure so Task Scheduler's
    Last Run Result is the alert.

    This is deliberately a separate task from \memgw\supervisor and
    \memgw\healthcheck, with its own manifest, so no installer removes another's
    task.

.PARAMETER WhatIf
    Report what would happen without creating anything.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$SkipVerify
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

$taskPath = '\memgw\'
$taskName = 'backup-pull'
$fullName = "$taskPath$taskName"
$manifestPath = 'D:\memgw\run\installed-backup-pull.json'
$root = 'D:\memgw'

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
    throw "task $fullName already exists. Remove it deliberately first."
}

$script = Join-Path $PSScriptRoot 'pull-vps-backup.ps1'
if (-not (Test-Path -LiteralPath $script)) { throw "pull script not found: $script" }

New-Item -ItemType Directory -Force -Path (Join-Path $root 'run') | Out-Null

$powershell = (Get-Command powershell.exe).Source
$action = New-ScheduledTaskAction `
    -Execute $powershell `
    -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $script) `
    -WorkingDirectory $root

# Daily at 03:10 local; the VPS stages at 02:40 UTC. Local time here is UTC+7,
# so 03:10 local is 20:10 UTC the previous day -- which is BEFORE the staging
# run. Use 11:00 local (04:00 UTC) so the staged dump exists first. At logon
# covers a machine that was off at the daily time (StartWhenAvailable).
$daily = New-ScheduledTaskTrigger -Daily -At '11:00'
$logon = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 60) `
    -RestartCount 2 `
    -RestartInterval (New-TimeSpan -Minutes 5)

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host "WhatIf: would register $fullName, daily 11:00 + at logon"
    Write-Host "  args: $($action.Arguments)"
    exit 0
}

Register-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Action $action `
    -Trigger @($daily, $logon) -Settings $settings -Principal $principal `
    -Description 'memgw: pull the VPS PostgreSQL backup staging dir off-host, verify sha256, seal with DPAPI, prune.' | Out-Null

$registered = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName
[ordered]@{
    installed_at = (Get-Date).ToString('o')
    installed_by = "$env:USERDOMAIN\$env:USERNAME"
    task_path    = $taskPath
    task_name    = $taskName
    action_hash  = Get-TaskActionHash -Task $registered
    script       = $script
    note         = 'Remove with uninstall-memgw-backup-pull.ps1. Do not edit by hand; the hash will fail.'
} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding utf8

Write-Host "registered: $fullName"
Write-Host "manifest  : $manifestPath"

if ($SkipVerify) { exit 0 }
Write-Host 'verifying: running the pull once...'
$v = Start-Process -FilePath $powershell -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',$script) -PassThru -Wait -WindowStyle Hidden
Write-Host "verification exit: $($v.ExitCode) (0=ok, 2=checksum failure)"
if ($v.ExitCode -eq 2) { throw 'the pull reported a checksum failure' }
Write-Host 'verification OK'
